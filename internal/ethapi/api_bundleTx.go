package ethapi

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/tracers/logger"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"
)

type BundleTxArgs struct {
	Txs []TransactionArgs
}
type BundleTxAccessList struct {
	AccessList *types.AccessList `json:"accessList"`
	GasUsed    uint64            `json:"gasUsed"`
	VmErr      string            `json:"vmErr"`
}
type BundleTxAccessListResult struct {
	AccessLists []BundleTxAccessList `json:"accessLists"`
	Err         string               `json:"err"`
}

// CreateAccessList creates a EIP-2930 type AccessList for the given transaction.
// Reexec and BlockNrOrHash can be specified to create the accessList on top of a certain state.
func (s *BlockChainAPI) CreateBundleTxAccessList(ctx context.Context, bundleTxArgs BundleTxArgs, blockNrOrHash *rpc.BlockNumberOrHash) (*BundleTxAccessListResult, error) {
	bNrOrHash := rpc.BlockNumberOrHashWithNumber(rpc.PendingBlockNumber)
	if blockNrOrHash != nil {
		bNrOrHash = *blockNrOrHash
	}
	acl, err := GetBundleTxAccessList(ctx, s.b, bNrOrHash, bundleTxArgs)
	if err != nil {
		return nil, err
	}
	aclResult := BundleTxAccessListResult{
		AccessLists: acl,
	}
	for i := 0; i < len(acl); i++ {
		if acl[i].VmErr != "" {
			aclResult.Err = acl[i].VmErr
		}
	}
	return &aclResult, nil
}

// AccessList creates an access list for the given transaction.
// If the accesslist creation fails an error is returned.
// If the transaction itself fails, an vmErr is returned.
func GetBundleTxAccessList(ctx context.Context, b Backend, blockNrOrHash rpc.BlockNumberOrHash, bundleTxArgs BundleTxArgs) ([]BundleTxAccessList, error) {
	// Retrieve the execution context
	db, header, err := b.StateAndHeaderByNumberOrHash(ctx, blockNrOrHash)
	if db == nil || err != nil {
		return nil, err
	}
	result := make([]BundleTxAccessList, 0)
	snapShotDb := db.Copy()
	for i := 0; i < len(bundleTxArgs.Txs); i++ {
		args := bundleTxArgs.Txs[i]

		// If the gas amount is not set, default to RPC gas cap.
		if args.Gas == nil {
			tmp := hexutil.Uint64(b.RPCGasCap())
			args.Gas = &tmp
		}

		// Ensure any missing fields are filled, extract the recipient and input data
		if err := args.setDefaults(ctx, b); err != nil {
			return nil, err
		}
		var to common.Address
		if args.To != nil {
			to = *args.To
		} else {
			to = crypto.CreateAddress(args.from(), uint64(*args.Nonce))
		}
		isPostMerge := header.Difficulty.Cmp(common.Big0) == 0
		// Retrieve the precompiles since they don't need to be added to the access list
		precompiles := vm.ActivePrecompiles(b.ChainConfig().Rules(header.Number, isPostMerge))

		// Create an initial tracer
		prevTracer := logger.NewAccessListTracer(nil, args.from(), to, precompiles)
		if args.AccessList != nil {
			prevTracer = logger.NewAccessListTracer(*args.AccessList, args.from(), to, precompiles)
		}
		for {
			// Retrieve the current access list to expand
			accessList := prevTracer.AccessList()
			log.Trace("Creating access list", "input", accessList)
			statedb := snapShotDb.Copy()
			// Copy the original db so we don't modify it

			// Set the accesslist to the last al
			args.AccessList = &accessList
			msg, err := args.ToMessage(b.RPCGasCap(), header.BaseFee)
			if err != nil {
				return nil, err
			}

			// Apply the transaction with the access list tracer
			tracer := logger.NewAccessListTracer(accessList, args.from(), to, precompiles)
			config := vm.Config{Tracer: tracer, Debug: true, NoBaseFee: true}
			vmenv, _, err := b.GetEVM(ctx, msg, statedb, header, &config)
			if err != nil {
				return nil, err
			}

			res, err := core.ApplyMessage(vmenv, msg, new(core.GasPool).AddGas(msg.Gas()))
			if err != nil {
				return nil, fmt.Errorf("failed to apply transaction: %v err: %v", args.toTransaction().Hash(), err)
			}

			if tracer.Equal(prevTracer) {
				//return accessList, res.UsedGas, res.Err, nil
				vmErr := ""
				if res.Err != nil {
					vmErr = res.Err.Error()
				}

				result = append(result, BundleTxAccessList{
					AccessList: &accessList,
					GasUsed:    res.UsedGas,
					VmErr:      vmErr,
				})
				snapShotDb = statedb
				break
			}
			prevTracer = tracer
		}
	}
	return result, nil
}

type TraceCallSimulationArgs struct {
	Args []TransactionArgs `json:"args"`
}

type BundleExecutionResult struct {
	UsedGas    uint64 // Total used gas but include the refunded gas
	Err        string // Any error encountered during the execution(listed in core/vm/errors.go)
	ReturnData []byte // Returned data from evm(function result or data supplied with revert opcode)
}
type BundleSimulationResult struct {
	Excute []*BundleExecutionResult `json:"excute"`
}

func (b *BlockChainAPI) GetBundleSimulation(ctx context.Context, args TraceCallSimulationArgs, blockNrOrHash rpc.BlockNumberOrHash, overrides *StateOverride) (*BundleSimulationResult, error) {
	result, _, err := TransactionsDoCall(ctx, b.b, args.Args, blockNrOrHash, overrides, b.b.RPCEVMTimeout(), b.b.RPCGasCap())
	if err != nil {
		return nil, err
	}
	res := BundleSimulationResult{
		Excute: make([]*BundleExecutionResult, 0),
	}
	for i := 0; i < len(result); i++ {
		if result[i] != nil {
			if result[i].Err != nil {
				res.Excute = append(res.Excute, &BundleExecutionResult{
					UsedGas:    result[i].UsedGas,
					Err:        result[i].Err.Error(),
					ReturnData: result[i].ReturnData,
				})
			} else {
				res.Excute = append(res.Excute, &BundleExecutionResult{
					UsedGas:    result[i].UsedGas,
					Err:        "",
					ReturnData: result[i].ReturnData,
				})
			}

		}
	}
	return &res, nil
}
