package tracers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/tracers/logger"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/rpc"
)

type TraceCallSimulationArgs struct {
	Args []ethapi.TransactionArgs `json:"args"`
}

func (api *API) TraceCallSimulation(ctx context.Context, args TraceCallSimulationArgs, blockNrOrHash rpc.BlockNumberOrHash, config *TraceCallConfig) ([]interface{}, error) {
	// Try to retrieve the specified block
	var (
		err   error
		block *types.Block
	)
	if hash, ok := blockNrOrHash.Hash(); ok {
		block, err = api.blockByHash(ctx, hash)
	} else if number, ok := blockNrOrHash.Number(); ok {
		if number == rpc.PendingBlockNumber {
			// We don't have access to the miner here. For tracing 'future' transactions,
			// it can be done with block- and state-overrides instead, which offers
			// more flexibility and stability than trying to trace on 'pending', since
			// the contents of 'pending' is unstable and probably not a true representation
			// of what the next actual block is likely to contain.
			return nil, errors.New("tracing on top of pending is not supported")
		}
		block, err = api.blockByNumber(ctx, number)
	} else {
		return nil, errors.New("invalid arguments; neither block nor hash specified")
	}
	if err != nil {
		return nil, err
	}
	// try to recompute the state
	reexec := defaultTraceReexec
	if config != nil && config.Reexec != nil {
		reexec = *config.Reexec
	}
	_statedb, release, err := api.backend.StateAtBlock(ctx, block, reexec, nil, true, false)
	if err != nil {
		return nil, err
	}
	defer release()
	snapShotDb := _statedb.Copy()
	vmctx := core.NewEVMBlockContext(block.Header(), api.chainContext(ctx), nil)
	// Apply the customization rules if required.
	if config != nil {
		if err := config.StateOverrides.Apply(snapShotDb); err != nil {
			return nil, err
		}
		config.BlockOverrides.Apply(&vmctx)
	}

	var traceConfig *TraceConfig
	if config != nil {
		traceConfig = &TraceConfig{
			Config:  config.Config,
			Tracer:  config.Tracer,
			Timeout: config.Timeout,
			Reexec:  config.Reexec,
		}
	}
	traceCallResult := make([]interface{}, 0)
	var (
		tracer Tracer
		//err       error
		timeout = defaultTraceTimeout
	)
	txctx := new(Context)
	tracer = logger.NewStructLogger(config.Config)
	if config.Tracer != nil {
		tracer, err = DefaultDirectory.New(*config.Tracer, txctx, config.TracerConfig)
		if err != nil {
			return nil, err
		}
	}

	// Define a meaningful timeout of a single transaction trace
	if config.Timeout != nil {
		if timeout, err = time.ParseDuration(*config.Timeout); err != nil {
			return nil, err
		}
	}
	for i := 0; i < len(args.Args); i++ {
		// Execute the trace
		msg, err := args.Args[i].ToMessage(api.backend.RPCGasCap(), block.BaseFee())
		if err != nil {
			return nil, err
		}
		txContext := core.NewEVMTxContext(msg)
		if traceConfig == nil {
			traceConfig = &TraceConfig{}
		}
		// Default tracer is the struct logger
		deadlineCtx, cancel := context.WithTimeout(ctx, timeout)
		go func() {
			<-deadlineCtx.Done()
			if errors.Is(deadlineCtx.Err(), context.DeadlineExceeded) {
				tracer.Stop(errors.New("execution timeout"))
			}
		}()
		defer cancel()
		statedb := snapShotDb.Copy()
		// Run the transaction with tracing enabled.
		vmenv := vm.NewEVM(vmctx, txContext, statedb, api.backend.ChainConfig(), vm.Config{Debug: true, Tracer: tracer, NoBaseFee: true})
		// Call Prepare to clear out the statedb access list
		statedb.SetTxContext(txctx.TxHash, txctx.TxIndex)
		if _, err = core.ApplyMessage(vmenv, msg, new(core.GasPool).AddGas(msg.GasLimit)); err != nil {
			return nil, fmt.Errorf("tracing failed: %w", err)
		}
		snapShotDb = statedb

		res, err := tracer.GetResult()
		if err != nil {
			return nil, err
		}
		traceCallResult = append(traceCallResult, res)
	}
	return traceCallResult, nil
}
