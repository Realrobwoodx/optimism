package fault

import (
	"context"
	"fmt"

	"github.com/ethereum-optimism/optimism/op-challenger/config"
	"github.com/ethereum-optimism/optimism/op-challenger/game/fault/contracts"
	"github.com/ethereum-optimism/optimism/op-challenger/game/fault/responder"
	"github.com/ethereum-optimism/optimism/op-challenger/game/fault/types"
	gameTypes "github.com/ethereum-optimism/optimism/op-challenger/game/types"
	"github.com/ethereum-optimism/optimism/op-challenger/metrics"
	"github.com/ethereum-optimism/optimism/op-service/sources/batching"
	"github.com/ethereum-optimism/optimism/op-service/txmgr"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/log"
)

type actor func(ctx context.Context) error

type GameInfo interface {
	GetStatus(context.Context) (gameTypes.GameStatus, error)
	GetClaimCount(context.Context) (uint64, error)
}

type GamePlayer struct {
	act                     actor
	agreeWithProposedOutput bool
	loader                  GameInfo
	logger                  log.Logger
	status                  gameTypes.GameStatus
}

type absolutePrestateValidator func(ctx context.Context, gameContract *contracts.FaultDisputeGameContract) error

type resourceCreator func(addr common.Address, gameDepth uint64, dir string) (types.TraceAccessor, types.OracleUpdater, absolutePrestateValidator, error)

func NewGamePlayer(
	ctx context.Context,
	logger log.Logger,
	m metrics.Metricer,
	cfg *config.Config,
	dir string,
	addr common.Address,
	txMgr txmgr.TxManager,
	client *ethclient.Client,
	creator resourceCreator,
) (*GamePlayer, error) {
	logger = logger.New("game", addr)
	loader, err := contracts.NewFaultDisputeGameContract(addr, batching.NewMultiCaller(client.Client(), batching.DefaultBatchSize))
	if err != nil {
		return nil, fmt.Errorf("failed to create fault dispute game contract wrapper: %w", err)
	}

	status, err := loader.GetStatus(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch game status: %w", err)
	}
	if status != gameTypes.GameStatusInProgress {
		logger.Info("Game already resolved", "status", status)
		// Game is already complete so skip creating the trace provider, loading game inputs etc.
		return &GamePlayer{
			logger:                  logger,
			loader:                  loader,
			agreeWithProposedOutput: cfg.AgreeWithProposedOutput,
			status:                  status,
			// Act function does nothing because the game is already complete
			act: func(ctx context.Context) error {
				return nil
			},
		}, nil
	}

	gameDepth, err := loader.GetMaxGameDepth(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch the game depth: %w", err)
	}

	provider, updater, prestateValidator, err := creator(addr, gameDepth, dir)
	if err != nil {
		return nil, fmt.Errorf("failed to create trace provider: %w", err)
	}

	if err := prestateValidator(ctx, loader); err != nil {
		return nil, fmt.Errorf("failed to validate absolute prestate: %w", err)
	}

	responder, err := responder.NewFaultResponder(logger, txMgr, addr)
	if err != nil {
		return nil, fmt.Errorf("failed to create the responder: %w", err)
	}

	return &GamePlayer{
		act:                     NewAgent(m, loader, int(gameDepth), provider, responder, updater, cfg.AgreeWithProposedOutput, logger).Act,
		agreeWithProposedOutput: cfg.AgreeWithProposedOutput,
		loader:                  loader,
		logger:                  logger,
		status:                  status,
	}, nil
}

func (g *GamePlayer) Status() gameTypes.GameStatus {
	return g.status
}

func (g *GamePlayer) ProgressGame(ctx context.Context) gameTypes.GameStatus {
	if g.status != gameTypes.GameStatusInProgress {
		// Game is already complete so don't try to perform further actions.
		g.logger.Trace("Skipping completed game")
		return g.status
	}
	g.logger.Trace("Checking if actions are required")
	if err := g.act(ctx); err != nil {
		g.logger.Error("Error when acting on game", "err", err)
	}
	status, err := g.loader.GetStatus(ctx)
	if err != nil {
		g.logger.Warn("Unable to retrieve game status", "err", err)
		return gameTypes.GameStatusInProgress
	}
	g.logGameStatus(ctx, status)
	g.status = status
	return status
}

func (g *GamePlayer) logGameStatus(ctx context.Context, status gameTypes.GameStatus) {
	if status == gameTypes.GameStatusInProgress {
		claimCount, err := g.loader.GetClaimCount(ctx)
		if err != nil {
			g.logger.Error("Failed to get claim count for in progress game", "err", err)
			return
		}
		g.logger.Info("Game info", "claims", claimCount, "status", status)
		return
	}
	var expectedStatus gameTypes.GameStatus
	if g.agreeWithProposedOutput {
		expectedStatus = gameTypes.GameStatusChallengerWon
	} else {
		expectedStatus = gameTypes.GameStatusDefenderWon
	}
	if expectedStatus == status {
		g.logger.Info("Game won", "status", status)
	} else {
		g.logger.Error("Game lost", "status", status)
	}
}
