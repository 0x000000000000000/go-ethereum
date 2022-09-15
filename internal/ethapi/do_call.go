package ethapi

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"
)

func TransactionDoCall(ctx context.Context, b Backend, args TransactionArgs, blockNrOrHash rpc.BlockNumberOrHash, overrides *StateOverride, timeout time.Duration, globalGasCap uint64) (*core.ExecutionResult, []*types.Log, error) {
	defer func(start time.Time) { log.Debug("Executing EVM call finished", "runtime", time.Since(start)) }(time.Now())
	state, header, err := b.StateAndHeaderByNumberOrHash(ctx, blockNrOrHash)
	if state == nil || err != nil {
		return nil, nil, err
	}
	if err := overrides.Apply(state); err != nil {
		return nil, nil, err
	}
	// Setup context so it may be cancelled the call has completed
	// or, in case of unmetered gas, setup a context with a timeout.
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	// Make sure the context is cancelled when the call has completed
	// this makes sure resources are cleaned up.
	defer cancel()

	// Get a new instance of the EVM.
	msg, err := args.ToMessage(globalGasCap, header.BaseFee)
	if err != nil {
		return nil, nil, err
	}
	evm, vmError, err := b.GetEVM(ctx, msg, state, header, &vm.Config{NoBaseFee: true})
	if err != nil {
		return nil, nil, err
	}
	// Wait for the context to be done and cancel the evm. Even if the
	// EVM has finished, cancelling may be done (repeatedly)
	go func() {
		<-ctx.Done()
		evm.Cancel()
	}()

	// Execute the message.
	gp := new(core.GasPool).AddGas(math.MaxUint64)
	//core.ApplyTransaction()
	result, err := core.ApplyMessage(evm, msg, gp)
	if err := vmError(); err != nil {
		return nil, nil, err
	}

	// If the timer caused an abort, return an appropriate error message
	if evm.Cancelled() {
		return nil, nil, fmt.Errorf("execution aborted (timeout = %v)", timeout)
	}
	if err != nil {
		return result, nil, fmt.Errorf("err: %w (supplied gas %d)", err, msg.Gas())
	}
	return result, state.Logs(), nil
}

func TransactionsDoCall(ctx context.Context, b Backend, args []TransactionArgs, blockNrOrHash rpc.BlockNumberOrHash, overrides *StateOverride, timeout time.Duration, globalGasCap uint64) ([]*core.ExecutionResult, [][]*types.Log, error) {
	defer func(start time.Time) { log.Debug("Executing EVM call finished", "runtime", time.Since(start)) }(time.Now())
	state, header, err := b.StateAndHeaderByNumberOrHash(ctx, blockNrOrHash)
	if state == nil || err != nil {
		return nil, nil, err
	}
	if err := overrides.Apply(state); err != nil {
		return nil, nil, err
	}
	// Setup context so it may be cancelled the call has completed
	// or, in case of unmetered gas, setup a context with a timeout.
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	// Make sure the context is cancelled when the call has completed
	// this makes sure resources are cleaned up.
	defer cancel()
	results := make([]*core.ExecutionResult, 0)
	logs := make([][]*types.Log, 0)
	logsPosition := 0
	for _, v := range args {
		// Get a new instance of the EVM.
		msg, err := v.ToMessage(globalGasCap, header.BaseFee)
		if err != nil {
			return nil, nil, err
		}
		evm, vmError, err := b.GetEVM(ctx, msg, state, header, &vm.Config{NoBaseFee: true})
		if err != nil {
			return nil, nil, err
		}
		// Wait for the context to be done and cancel the evm. Even if the
		// EVM has finished, cancelling may be done (repeatedly)
		go func() {
			<-ctx.Done()
			evm.Cancel()
		}()

		// Execute the message.
		gp := new(core.GasPool).AddGas(math.MaxUint64)
		//core.ApplyTransaction()
		result, err := core.ApplyMessage(evm, msg, gp)
		if err := vmError(); err != nil {
			return nil, nil, err
		}

		// If the timer caused an abort, return an appropriate error message
		if evm.Cancelled() {
			return nil, nil, fmt.Errorf("execution aborted (timeout = %v)", timeout)
		}
		if err != nil {
			return []*core.ExecutionResult{result}, nil, fmt.Errorf("err: %w (supplied gas %d)", err, msg.Gas())
		}
		results = append(results, result)

		logs = append(logs, state.Logs()[logsPosition:len(state.Logs())])
		logsPosition = len(state.Logs())
	}
	return results, logs, nil
}

func (b *BlockChainAPI) PredictDoCall(ctx context.Context, tx types.Transaction, blockNrOrHash rpc.BlockNumberOrHash, overrides *StateOverride) (hexutil.Bytes, []*types.Log, error) {
	chainId := b.b.ChainConfig().ChainID
	msg, err := tx.AsMessage(types.NewLondonSigner(chainId), nil)
	from := msg.From()
	gas := hexutil.Uint64(tx.Gas())
	gasPrice := hexutil.Big(*tx.GasPrice())
	value := hexutil.Big(*tx.Value())
	nonce := hexutil.Uint64(tx.Nonce())
	data := hexutil.Bytes(tx.Data())
	hexChainId := hexutil.Big(*chainId)
	args := TransactionArgs{
		From:     &from,
		To:       tx.To(),
		Gas:      &gas,
		GasPrice: &gasPrice,
		Value:    &value,
		Nonce:    &nonce,
		Data:     &data,
		Input:    &data,
		ChainID:  &hexChainId,
	}
	result, logs, err := TransactionDoCall(ctx, b.b, args, blockNrOrHash, overrides, b.b.RPCEVMTimeout(), b.b.RPCGasCap())
	if err != nil {
		log.Error("PredictDoCall err.....", err.Error())
		return nil, nil, err
	}
	// If the result contains a revert reason, try to unpack and return it.
	if len(result.Revert()) > 0 {
		return nil, nil, newRevertError(result)
	}
	return result.Return(), logs, result.Err
}

func (b *BlockChainAPI) TransactionsPredictDoCall(ctx context.Context, txs []*types.Transaction, blockNrOrHash rpc.BlockNumberOrHash, overrides *StateOverride) ([]hexutil.Bytes, [][]*types.Log, []error) {
	args := make([]TransactionArgs, 0)
	for _, v := range txs {
		chainId := b.b.ChainConfig().ChainID
		msg, err := v.AsMessage(types.NewLondonSigner(chainId), nil)
		if err != nil {
			return nil, nil, []error{err}
		}
		from := msg.From()
		gas := hexutil.Uint64(v.Gas())
		gasPrice := hexutil.Big(*v.GasPrice())
		value := hexutil.Big(*v.Value())
		nonce := hexutil.Uint64(v.Nonce())
		data := hexutil.Bytes(v.Data())
		hexChainId := hexutil.Big(*chainId)
		Txargs := TransactionArgs{
			From:     &from,
			To:       v.To(),
			Gas:      &gas,
			GasPrice: &gasPrice,
			Value:    &value,
			Nonce:    &nonce,
			Data:     &data,
			Input:    &data,
			ChainID:  &hexChainId,
		}
		args = append(args, Txargs)
	}

	result, logs, err := TransactionsDoCall(ctx, b.b, args, blockNrOrHash, overrides, b.b.RPCEVMTimeout(), b.b.RPCGasCap())
	if err != nil {
		log.Error("TransactionsPredictDoCall err.....", err.Error())
		return nil, nil, []error{err}
	}
	returns := make([]hexutil.Bytes, 0)
	errs := make([]error, 0)
	// If the result contains a revert reason, try to unpack and return it.
	for i := 0; i < len(result); i++ {
		if len(result[i].Revert()) > 0 {
			return nil, nil, []error{newRevertError(result[i])}
		}
		returns = append(returns, result[i].Return())
		errs = append(errs, result[i].Err)
	}
	return returns, logs, errs
}

type RPCTransactionPlus struct {
	Tx     *RPCTransaction `json:"tx"`
	Logs   []*types.Log    `json:"logs"`
	Result string          `json:"result"`
	Revert string          `json:"revert"`
}

func (s *TransactionAPI) SetBlockChainAPI(bc *BlockChainAPI) {
	s.bc = bc
}

// GetTransactionByHash returns the transaction for the given hash
func (s *TransactionAPI) GetTransactionByHashAndPredictDoCall(ctx context.Context, hash common.Hash) (*RPCTransactionPlus, error) {
	// Try to return an already finalized transaction
	tx, blockHash, blockNumber, index, err := s.b.GetTransaction(ctx, hash)
	if err != nil {
		return nil, err
	}
	if tx != nil {
		header, err := s.b.HeaderByHash(ctx, blockHash)
		if err != nil {
			return nil, err
		}
		tp := newRPCTransaction(tx, blockHash, blockNumber, index, header.BaseFee, s.b.ChainConfig())

		return &RPCTransactionPlus{
			Tx: tp,
		}, nil
	}
	// No finalized transaction, try to retrieve it from the pool
	if tx := s.b.GetPoolTransaction(hash); tx != nil {
		block := s.b.CurrentBlock()

		blockNum := rpc.BlockNumber(block.Number().Int64())
		blackHash := block.Hash()
		blockorhash := rpc.BlockNumberOrHash{
			BlockNumber:      &blockNum,
			BlockHash:        &blackHash,
			RequireCanonical: false,
		}

		result, logs, revertErr := s.bc.PredictDoCall(ctx, *tx, blockorhash, nil)
		tp := newRPCPendingTransaction(tx, s.b.CurrentHeader(), s.b.ChainConfig())
		txResult := &RPCTransactionPlus{
			Tx:     tp,
			Logs:   logs,
			Result: result.String(),
		}
		if revertErr != nil {
			txResult.Revert = revertErr.Error()
		}
		return txResult, nil
	}
	// Transaction unknown, return as such
	return nil, nil
}

// GetTransactionByHash returns the transaction for the given hash
func (s *TransactionAPI) GetBoundTransactionsAndPredictDoCall(ctx context.Context, hash *common.Hash, input hexutil.Bytes) ([]*RPCTransactionPlus, error) {
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(input); err != nil {
		return nil, err
	}

	block := s.b.CurrentBlock()
	blockNum := rpc.BlockNumber(block.Number().Int64())
	blackHash := block.Hash()
	blockorhash := rpc.BlockNumberOrHash{
		BlockNumber:      &blockNum,
		BlockHash:        &blackHash,
		RequireCanonical: false,
	}
	totalTxs := make([]*types.Transaction, 0)
	if hash != nil {
		if tx := s.b.GetPoolTransaction(*hash); tx != nil {
			totalTxs = append(totalTxs, tx)
		}

	}
	totalTxs = append(totalTxs, tx)
	result, logs, revertErr := s.bc.TransactionsPredictDoCall(ctx, totalTxs, blockorhash, nil)
	txResults := make([]*RPCTransactionPlus, 0)
	for i := 0; i < len(totalTxs); i++ {
		tp := newRPCPendingTransaction(totalTxs[i], s.b.CurrentHeader(), s.b.ChainConfig())
		txResult := &RPCTransactionPlus{
			Tx:     tp,
			Logs:   logs[i],
			Result: result[i].String(),
		}
		if revertErr[i] != nil {
			txResult.Revert = revertErr[i].Error()
		}
		txResults = append(txResults, txResult)
	}
	return txResults, nil
}

func (s *TransactionAPI) DebugTxHashAndPeerInfo(ctx context.Context, open bool, minDiffTime string) {
	TxsWithPeersInfo = open
	minDiffTimeInt, err := strconv.Atoi(minDiffTime)
	if err != nil {
		log.Error("DebugTxHashAndPeerInfo", "err", err)
		return
	}
	MinDiffTime = int64(minDiffTimeInt)
	log.Info("TxsWithPeersInfo", "TxsWithPeersInfo", open, "MinDiffTime", MinDiffTime)
}

func (s *TransactionAPI) DebugBlockAndPeerInfo(ctx context.Context, open bool, minDiffTime string) {
	BlockWithPeersInfo = open
	blockMinDiffTime, err := strconv.Atoi(minDiffTime)
	if err != nil {
		log.Error("DebugBlockAndPeerInfo", "err", err)
		return
	}
	BlockMinDiffTime = int64(blockMinDiffTime)
	log.Info("BlockWithPeersInfo", "BlockWithPeersInfo", open, "BlockMinDiffTime", BlockMinDiffTime)
}

func (s *TransactionAPI) GetPeerListInfo(ctx context.Context) map[string]uint64 {
	return GetPeerListInfo().Peers

}

var TxsWithPeersInfo = false
var MinDiffTime int64 = 0

var BlockWithPeersInfo = false
var BlockMinDiffTime int64 = 0

type TransactionInfo struct {
	Hash            string
	TxTime          int64
	ReceiveTime     int64
	Diff            int64
	BlockNumber     uint64
	BlockTime       uint64
	DiffByBlockTime int64
}

type PeerListInfo struct {
	Peers map[string]uint64
	Mux   sync.Mutex
}

var once sync.Once
var PeerList *PeerListInfo

func GetPeerListInfo() *PeerListInfo {
	once.Do(func() {
		PeerList = &PeerListInfo{
			Peers: make(map[string]uint64),
			Mux:   sync.Mutex{},
		}
	})
	return PeerList
}

func (peerlist *PeerListInfo) PrintlnTxsWithPeersInfo(peer string, txs []*types.Transaction, parentBlock *types.Block, currentBlock *types.Block) {
	if TxsWithPeersInfo && len(txs) > 0 && parentBlock != nil && currentBlock != nil {
		txs_info := make([]TransactionInfo, 0)
		now := time.Now().UTC().Unix()
		diffByBlockTime := now - int64(parentBlock.Time())
		for _, v := range txs {
			txTime := v.Time().Unix()
			if diffByBlockTime <= MinDiffTime {
				if v.GasFeeCap() != nil && currentBlock.BaseFee() != nil {
					if v.GasFeeCap().Cmp(currentBlock.BaseFee()) >= 0 {
						txs_info = append(txs_info, TransactionInfo{
							Hash:            v.Hash().String(),
							TxTime:          txTime,
							ReceiveTime:     now,
							Diff:            now - txTime,
							BlockNumber:     parentBlock.NumberU64(),
							BlockTime:       parentBlock.Time(),
							DiffByBlockTime: diffByBlockTime,
						})
					}
				}

			}

		}
		if len(txs_info) > 0 {
			peerlist.Mux.Lock()
			peerlist.Peers[peer] = peerlist.Peers[peer] + uint64(len(txs_info))
			peerlist.Mux.Unlock()
			log.Info("peer", "@PeerId", peer, "@txs", txs_info, "effective", len(txs)/len(txs_info))
		}
	}
}

func (peerlist *PeerListInfo) PrintlnBlockWithPeersInfo(peer string, block *types.Block) {
	if BlockWithPeersInfo && block != nil {
		now := time.Now().UTC().Unix()
		diffByBlockTime := now - int64(block.Time())
		if diffByBlockTime <= BlockMinDiffTime {
			log.Info("peer", "@PeerId", peer, "@blockTime", block.Time(), "@diff", diffByBlockTime)
		}
	}
}
