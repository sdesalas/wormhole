package guardiand

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/certusone/wormhole/node/pkg/db"
	publicrpcv1 "github.com/certusone/wormhole/node/pkg/proto/publicrpc/v1"
	"github.com/certusone/wormhole/node/pkg/publicrpc"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"math"
	"net"
	"os"

	"github.com/certusone/wormhole/node/pkg/common"
	nodev1 "github.com/certusone/wormhole/node/pkg/proto/node/v1"
	"github.com/certusone/wormhole/node/pkg/supervisor"
	"github.com/certusone/wormhole/node/pkg/vaa"
)

type nodePrivilegedService struct {
	nodev1.UnimplementedNodePrivilegedServiceServer
	db      *db.Database
	injectC chan<- *vaa.VAA
	logger  *zap.Logger
}

// adminGuardianSetUpdateToVAA converts a nodev1.GuardianSetUpdate message to its canonical VAA representation.
// Returns an error if the data is invalid.
func adminGuardianSetUpdateToVAA(req *nodev1.GuardianSetUpdate, guardianSetIndex uint32, nonce uint32, sequence uint64) (*vaa.VAA, error) {
	if len(req.Guardians) == 0 {
		return nil, errors.New("empty guardian set specified")
	}

	if len(req.Guardians) > common.MaxGuardianCount {
		return nil, fmt.Errorf("too many guardians - %d, maximum is %d", len(req.Guardians), common.MaxGuardianCount)
	}

	addrs := make([]ethcommon.Address, len(req.Guardians))
	for i, g := range req.Guardians {
		if !ethcommon.IsHexAddress(g.Pubkey) {
			return nil, fmt.Errorf("invalid pubkey format at index %d (%s)", i, g.Name)
		}

		ethAddr := ethcommon.HexToAddress(g.Pubkey)
		for j, pk := range addrs {
			if pk == ethAddr {
				return nil, fmt.Errorf("duplicate pubkey at index %d (duplicate of %d): %s", i, j, g.Name)
			}
		}

		addrs[i] = ethAddr
	}

	v := vaa.CreateGovernanceVAA(nonce, sequence, guardianSetIndex,
		vaa.BodyGuardianSetUpdate{
			Keys:     addrs,
			NewIndex: guardianSetIndex + 1,
		}.Serialize())

	return v, nil
}

// adminContractUpgradeToVAA converts a nodev1.ContractUpgrade message to its canonical VAA representation.
// Returns an error if the data is invalid.
func adminContractUpgradeToVAA(req *nodev1.ContractUpgrade, guardianSetIndex uint32, nonce uint32, sequence uint64) (*vaa.VAA, error) {
	b, err := hex.DecodeString(req.NewContract)
	if err != nil {
		return nil, errors.New("invalid new contract address encoding (expected hex)")
	}

	if len(b) != 32 {
		return nil, errors.New("invalid new_contract address")
	}

	if req.ChainId > math.MaxUint16 {
		return nil, errors.New("invalid chain_id")
	}

	newContractAddress := vaa.Address{}
	copy(newContractAddress[:], req.NewContract)

	v := vaa.CreateGovernanceVAA(nonce, sequence, guardianSetIndex,
		vaa.BodyContractUpgrade{
			ChainID:     vaa.ChainID(req.ChainId),
			NewContract: newContractAddress,
		}.Serialize())

	return v, nil
}

// tokenBridgeRegisterChain converts a nodev1.TokenBridgeRegisterChain message to its canonical VAA representation.
// Returns an error if the data is invalid.
func tokenBridgeRegisterChain(req *nodev1.BridgeRegisterChain, guardianSetIndex uint32, nonce uint32, sequence uint64) (*vaa.VAA, error) {
	if req.ChainId > math.MaxUint16 {
		return nil, errors.New("invalid chain_id")
	}

	b, err := hex.DecodeString(req.EmitterAddress)
	if err != nil {
		return nil, errors.New("invalid emitter address encoding (expected hex)")
	}

	if len(b) != 32 {
		return nil, errors.New("invalid emitter address (expected 32 bytes)")
	}

	emitterAddress := vaa.Address{}
	copy(emitterAddress[:], b)

	v := vaa.CreateGovernanceVAA(nonce, sequence, guardianSetIndex,
		vaa.BodyTokenBridgeRegisterChain{
			Module:         req.Module,
			ChainID:        vaa.ChainID(req.ChainId),
			EmitterAddress: emitterAddress,
		}.Serialize())

	return v, nil
}

// tokenBridgeUpgradeContract converts a nodev1.TokenBridgeRegisterChain message to its canonical VAA representation.
// Returns an error if the data is invalid.
func tokenBridgeUpgradeContract(req *nodev1.BridgeUpgradeContract, guardianSetIndex uint32, nonce uint32, sequence uint64) (*vaa.VAA, error) {
	if req.TargetChainId > math.MaxUint16 {
		return nil, errors.New("invalid target_chain_id")
	}

	b, err := hex.DecodeString(req.NewContract)
	if err != nil {
		return nil, errors.New("invalid new contract address (expected hex)")
	}

	if len(b) != 32 {
		return nil, errors.New("invalid new contract address (expected 32 bytes)")
	}

	newContract := vaa.Address{}
	copy(newContract[:], b)

	v := vaa.CreateGovernanceVAA(nonce, sequence, guardianSetIndex,
		vaa.BodyTokenBridgeUpgradeContract{
			Module:        req.Module,
			TargetChainID: vaa.ChainID(req.TargetChainId),
			NewContract:   newContract,
		}.Serialize())

	return v, nil
}

func (s *nodePrivilegedService) InjectGovernanceVAA(ctx context.Context, req *nodev1.InjectGovernanceVAARequest) (*nodev1.InjectGovernanceVAAResponse, error) {
	s.logger.Info("governance VAA injected via admin socket", zap.String("request", req.String()))

	var (
		v   *vaa.VAA
		err error
	)

	digests := make([][]byte, len(req.Messages))

	for i, message := range req.Messages {
		switch payload := message.Payload.(type) {
		case *nodev1.GovernanceMessage_GuardianSet:
			v, err = adminGuardianSetUpdateToVAA(payload.GuardianSet, req.CurrentSetIndex, message.Nonce, message.Sequence)
		case *nodev1.GovernanceMessage_ContractUpgrade:
			v, err = adminContractUpgradeToVAA(payload.ContractUpgrade, req.CurrentSetIndex, message.Nonce, message.Sequence)
		case *nodev1.GovernanceMessage_BridgeRegisterChain:
			v, err = tokenBridgeRegisterChain(payload.BridgeRegisterChain, req.CurrentSetIndex, message.Nonce, message.Sequence)
		case *nodev1.GovernanceMessage_BridgeContractUpgrade:
			v, err = tokenBridgeUpgradeContract(payload.BridgeContractUpgrade, req.CurrentSetIndex, message.Nonce, message.Sequence)
		default:
			panic(fmt.Sprintf("unsupported VAA type: %T", payload))
		}
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}

		// Generate digest of the unsigned VAA.
		digest, err := v.SigningMsg()
		if err != nil {
			panic(err)
		}

		s.logger.Info("governance VAA constructed",
			zap.Any("vaa", v),
			zap.String("digest", digest.String()),
		)

		s.injectC <- v

		digests[i] = digest.Bytes()
	}

	return &nodev1.InjectGovernanceVAAResponse{Digests: digests}, nil
}

func (s *nodePrivilegedService) FindMissingMessages(ctx context.Context, req *nodev1.FindMissingMessagesRequest) (*nodev1.FindMissingMessagesResponse, error) {
	b, err := hex.DecodeString(req.EmitterAddress)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid emitter address encoding: %v", err)
	}
	emitterAddress := vaa.Address{}
	copy(emitterAddress[:], b)

	ids, first, last, err := s.db.FindEmitterSequenceGap(db.VAAID{
		EmitterChain:   vaa.ChainID(req.EmitterChain),
		EmitterAddress: emitterAddress,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "database operation failed: %v", err)
	}

	resp := make([]string, len(ids))
	for i, v := range ids {
		resp[i] = fmt.Sprintf("%d/%s/%d", req.EmitterChain, emitterAddress, v)
	}
	return &nodev1.FindMissingMessagesResponse{
		MissingMessages: resp,
		FirstSequence:   first,
		LastSequence:    last,
	}, nil
}

func adminServiceRunnable(logger *zap.Logger, socketPath string, injectC chan<- *vaa.VAA, db *db.Database, gst *common.GuardianSetState) (supervisor.Runnable, error) {
	// Delete existing UNIX socket, if present.
	fi, err := os.Stat(socketPath)
	if err == nil {
		fmode := fi.Mode()
		if fmode&os.ModeType == os.ModeSocket {
			err = os.Remove(socketPath)
			if err != nil {
				return nil, fmt.Errorf("failed to remove existing socket at %s: %w", socketPath, err)
			}
		} else {
			return nil, fmt.Errorf("%s is not a UNIX socket", socketPath)
		}
	}

	// Create a new UNIX socket and listen to it.

	// The socket is created with the default umask. We set a restrictive umask in setRestrictiveUmask
	// to ensure that any files we create are only readable by the user - this is much harder to mess up.
	// The umask avoids a race condition between file creation and chmod.

	laddr, err := net.ResolveUnixAddr("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("invalid listen address: %v", err)
	}
	l, err := net.ListenUnix("unix", laddr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on %s: %w", socketPath, err)
	}

	logger.Info("admin server listening on", zap.String("path", socketPath))

	nodeService := &nodePrivilegedService{
		injectC: injectC,
		db:      db,
		logger:  logger.Named("adminservice"),
	}

	publicrpcService := publicrpc.NewPublicrpcServer(logger, db, gst)

	grpcServer := common.NewInstrumentedGRPCServer(logger)
	nodev1.RegisterNodePrivilegedServiceServer(grpcServer, nodeService)
	publicrpcv1.RegisterPublicRPCServiceServer(grpcServer, publicrpcService)
	return supervisor.GRPCServer(grpcServer, l, false), nil
}
