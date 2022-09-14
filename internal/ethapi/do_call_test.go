package ethapi

import (
	"math/big"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/assert"
)

func TestPrintlnTxsWithPeersInfo(t *testing.T) {
	header := new(types.Header)
	header.Time = uint64(time.Now().Unix()) + 10
	header.BaseFee = big.NewInt(1)
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("could not generate key: %v", err)
	}
	txMeta := types.NewTx(&types.DynamicFeeTx{
		Nonce:     0,
		To:        &common.Address{},
		Value:     big.NewInt(100),
		Gas:       100,
		GasFeeCap: big.NewInt(int64(1000000)),
		GasTipCap: big.NewInt(int64(rand.Intn(100000))),
		Data:      nil,
	})
	signer := types.LatestSignerForChainID(common.Big1)
	tx, err := types.SignTx(txMeta, signer, key)
	if err != nil {
		t.Fatalf("SignTx failed: %v", err)
	}
	block := types.NewBlock(header, nil, nil, nil, nil)
	peerlistInfo := GetPeerListInfo()
	TxsWithPeersInfo = true
	MinDiffTime = 100
	count := 200
	var group sync.WaitGroup
	group.Add(count)
	for i := 0; i < count; i++ {
		go func() {
			peerlistInfo.PrintlnTxsWithPeersInfo("node1", []*types.Transaction{tx}, block, block)
			group.Done()
		}()
	}
	group.Wait()
	assert.Equal(t, peerlistInfo.Peers["node1"], uint64(count))

}
