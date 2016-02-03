// Copyright (c) 2016 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package rpctest

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	rpc "github.com/btcsuite/btcrpcclient"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	_ "github.com/btcsuite/btcwallet/walletdb/bdb" // Required to register boltdb.
)

var (
	// tempDataDir is the name of the temporary directory used by the test harness.
	tempDataDir = "testnode"

	// current number of active test nodes.
	numTestInstances = 0

	defaultP2pPort = 18555
	defaultRPCPort = 18556

	// Used to protest concurrent access to above declared variables.
	testCreationLock sync.Mutex
)

var testInstances []*Harness

// TODO(roasbeef): global slice with list of all active states.

// Harness ...
type Harness struct {
	ActiveNet *chaincfg.Params

	Node     *rpc.Client
	node     *node
	handlers *rpc.NotificationHandlers

	Wallet       *wallet.Wallet
	chainClient  *chain.RPCClient
	coinbaseKey  *btcec.PrivateKey
	coinbaseAddr btcutil.Address

	testNodeDir    string
	maxConnRetries int
}

// New creates and initializes new instance of the rpc test harness.
// Optionally, websocket handlers and a specified configuration may be passed.
// In the case that a nil config is passed, a default configuration will be used.
func New(activeNet *chaincfg.Params, handlers *rpc.NotificationHandlers, extraArgs []string) (*Harness, error) {
	testCreationLock.Lock()
	defer testCreationLock.Unlock()

	nodeTestData := tempDataDir + strconv.Itoa(int(numTestInstances))
	certFile := filepath.Join(nodeTestData, "rpc.cert")
	keyFile := filepath.Join(nodeTestData, "rpc.key")

	// Create folder to store our tls info.
	if err := os.Mkdir(nodeTestData, 0700); err != nil {
		return nil, err
	}

	// Generate the default config if needed.
	if err := genCertPair(certFile, keyFile); err != nil {
		return nil, err
	}

	// Since this btcd process which will eventually be created by this
	// Harness is running in simnet mode, we'll be able to easily generate
	// blocks. So we generate a fresh private key to use for our coinbase
	// payouts. This private key will also be imported into the wallet so
	// tests are able to move coins around at will.
	coinbaseAddr, coinbaseKey, err := generateCoinbasePayout(activeNet)
	if err != nil {
		return nil, err
	}
	miningAddr := fmt.Sprintf("--miningaddr=%s", coinbaseAddr)
	extraArgs = append(extraArgs, miningAddr)

	config, err := newConfig("rpctest", certFile, keyFile, extraArgs)
	if err != nil {
		return nil, err
	}

	// Generate p2p+rpc listening addresses.
	p2p, rpc := generateListeningAddresses()
	config.listen = p2p
	config.rpcListen = rpc

	// Create the testing node bounded to the simnet.
	node, err := newNode(config, nodeTestData)
	if err != nil {
		return nil, err
	}

	numTestInstances++

	// TODO(Roasbeef):
	//  * joinNodes(joinType)
	//  * syncBLocks + syncMempools?
	//  * connectAllNodes?

	h := &Harness{
		handlers:       handlers,
		node:           node,
		maxConnRetries: 20,
		testNodeDir:    nodeTestData,
		coinbaseKey:    coinbaseKey,
		coinbaseAddr:   coinbaseAddr,
		ActiveNet:      activeNet,
	}

	testInstances = append(testInstances, h)

	return h, nil
}

// SetUp initializes the rpc test state. Initialization includes: starting up a
// simnet node, creating a websocket client and connecting to the started node,
// and finally: optionally generating and submitting a testchain with a configurable
// number of mature coinbase outputs coinbase outputs.
func (h *Harness) SetUp(createTestChain bool, numMatureOutputs uint32) error {
	var err error

	// Start the btcd node itself. This spawns a new process which will be
	// managed
	if err = h.node.start(); err != nil {
		return err
	}
	if err := h.connectRPCClient(); err != nil {
		return err
	}

	// Create a test chain with the desired number of mature coinbase
	// outputs.
	if createTestChain {
		numToGenerate := blockchain.CoinbaseMaturity + numMatureOutputs
		_, err := h.Node.Generate(numToGenerate)
		if err != nil {
			return err
		}
	}

	netDir := filepath.Join(h.testNodeDir, h.ActiveNet.Name)
	walletLoader := wallet.NewLoader(h.ActiveNet, netDir)

	h.Wallet, err = walletLoader.CreateNewWallet([]byte("pub"),
		[]byte("password"), nil)
	if err != nil {
		return err
	}
	if err := h.Wallet.Manager.Unlock([]byte("password")); err != nil {
		return err
	}

	rpcConf := h.node.config.rpcConnConfig()
	rpcc, err := chain.NewRPCClient(h.ActiveNet, rpcConf.Host, rpcConf.User,
		rpcConf.Pass, rpcConf.Certificates, false, 20)
	if err != nil {
		return err
	}

	// Start the goroutines in the underlying wallet.
	h.chainClient = rpcc
	if err := h.chainClient.Start(); err != nil {
		return err
	}
	h.Wallet.Start()

	// Encode our coinbase private key in WIF format, then import it into
	// the wallet so we'll be able to generate spends, and update the
	// balance of the wallet as blocks are generated.
	wif, err := btcutil.NewWIF(h.coinbaseKey, h.ActiveNet, true)
	if err != nil {
		return err
	}
	if _, err := h.Wallet.ImportPrivateKey(wif, nil, false); err != nil {
		return err
	}

	h.Wallet.SynchronizeRPC(rpcc)

	// Wait for the wallet to sync up to the current height.
	ticker := time.NewTicker(time.Millisecond * 100)
out:
	// TODO(roasbeef): use wallet's notification server here instead?
	for {
		select {
		case <-ticker.C:
			if h.Wallet.Manager.SyncedTo().Height == 125 {
				break out
			}
		}
	}
	ticker.Stop()

	// Now that the wallet has synced up, submit a re-scan, blocking until
	// it's finished.
	if err := h.Wallet.Rescan([]btcutil.Address{h.coinbaseAddr}, nil); err != nil {
		return err
	}

	return nil
}

// TearDown stops the running rpc test instance. All created processes are
// killed, and temporary directories removed.
func (h *Harness) TearDown() error {
	if h.Node != nil {
		h.Node.Shutdown()
	}

	if h.Wallet != nil {
		h.Wallet.Stop()
	}
	if h.chainClient != nil {
		h.chainClient.Shutdown()
	}

	if err := h.node.shutdown(); err != nil {
		return err
	}

	if err := os.RemoveAll(h.testNodeDir); err != nil {
		return err
	}

	return nil
}

// connectRPCClient attempts to establish an RPC connection to the created
// btcd process belonging to this Harness instance. If the initial connection
// attempt fails, this function will retry h.maxConnRetries times, backing off
// the time between subsequent attempts. If after h.maxConnRetries attempts,
// we're not able to establish a connection, this function returns with an error.
func (h *Harness) connectRPCClient() error {
	var client *rpc.Client
	var err error

	rpcConf := h.node.config.rpcConnConfig()
	for i := 0; i < h.maxConnRetries; i++ {
		if client, err = rpc.New(&rpcConf, h.handlers); err != nil {
			time.Sleep(time.Duration(i) * 50 * time.Millisecond)
			continue
		}
		break
	}

	if client == nil {
		return fmt.Errorf("connection timedout")
	}

	h.Node = client
	return nil
}

// CoinbaseSpend creates, signs, and finally broadcasts a transaction spending
// the harness' available mature coinbase outputs creating new outputs according
// to targetOutputs. targetOutputs maps a string encoding of a Bitcoin address,
// to the amount of coins which should be created for that output.
func (h *Harness) CoinbaseSpend(targetOutputs map[string]btcutil.Amount) (*wire.ShaHash, error) {
	return h.Wallet.SendPairs(targetOutputs, waddrmgr.ImportedAddrAccount, 1)
}

// RPCConfig ...
func (h *Harness) RPCConfig() rpc.ConnConfig {
	return h.node.config.rpcConnConfig()
}

// generateListeningAddresses returns two strings representing listening
// addresses designated for the current rpc test. If there haven't been any
// test instances created, the default ports are used. Otherwise, in order to
// support multiple test nodes running at once, the p2p and rpc port are
// incremented after each initialization.
func generateListeningAddresses() (string, string) {
	var p2p, rpc string
	localhost := "127.0.0.1"
	if numTestInstances == 0 {
		p2p = net.JoinHostPort(localhost, strconv.Itoa(defaultP2pPort))
		rpc = net.JoinHostPort(localhost, strconv.Itoa(defaultRPCPort))
	} else {
		p2p = net.JoinHostPort(localhost,
			strconv.Itoa(defaultP2pPort+numTestInstances))
		rpc = net.JoinHostPort(localhost,
			strconv.Itoa(defaultRPCPort+numTestInstances))
	}

	return p2p, rpc
}

// generateCoinbasePayout generates a fresh private key, and the corresponding
// p2pkh address for use within all coinbase outputs produced for an instance
// of the test harness.
func generateCoinbasePayout(net *chaincfg.Params) (btcutil.Address, *btcec.PrivateKey, error) {
	privKey, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return nil, nil, err
	}

	addr, err := btcutil.NewAddressPubKey(privKey.PubKey().SerializeCompressed(),
		net)
	if err != nil {
		return nil, nil, err
	}

	return addr.AddressPubKeyHash(), privKey, nil
}