package seal_migration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/go-test/deep"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/vault/helper/namespace"
	"github.com/hashicorp/vault/helper/testhelpers"
	"github.com/hashicorp/vault/helper/testhelpers/teststorage"
	vaulthttp "github.com/hashicorp/vault/http"
	"github.com/hashicorp/vault/physical/raft"
	"github.com/hashicorp/vault/sdk/helper/logging"
	"github.com/hashicorp/vault/vault"
)

func TestShamir(t *testing.T) {
	testVariousBackends(t, testShamir)
}

func testVariousBackends(t *testing.T, tf testFunc) {

	logger := logging.NewVaultLogger(hclog.Debug).Named(t.Name())

	t.Run("inmem", func(t *testing.T) {
		t.Parallel()

		logger := logger.Named("inmem")
		storage, cleanup := teststorage.MakeReusableStorage(
			t, logger, teststorage.MakeInmemBackend(t, logger))
		defer cleanup()
		tf(t, logger, storage, 51000)
	})

	t.Run("file", func(t *testing.T) {
		t.Parallel()

		logger := logger.Named("file")
		storage, cleanup := teststorage.MakeReusableStorage(
			t, logger, teststorage.MakeFileBackend(t, logger))
		defer cleanup()
		tf(t, logger, storage, 52000)
	})

	t.Run("consul", func(t *testing.T) {
		t.Parallel()

		logger := logger.Named("consul")
		storage, cleanup := teststorage.MakeReusableStorage(
			t, logger, teststorage.MakeConsulBackend(t, logger))
		defer cleanup()
		tf(t, logger, storage, 53000)
	})

	t.Run("raft", func(t *testing.T) {
		t.Parallel()

		logger := logger.Named("raft")
		storage, cleanup := teststorage.MakeReusableRaftStorage(t, logger)
		defer cleanup()
		tf(t, logger, storage, 54000)
	})
}

type testFunc func(t *testing.T, logger hclog.Logger, storage teststorage.ReusableStorage, basePort int)

func testShamir(
	t *testing.T, logger hclog.Logger,
	storage teststorage.ReusableStorage, basePort int) {

	rootToken, keys := initializeShamir(t, logger, storage, basePort)
	reuseShamir(t, logger, storage, basePort, rootToken, keys)
}

// initializeShamir initializes a brand new backend storage with Shamir.
func initializeShamir(
	t *testing.T, logger hclog.Logger,
	storage teststorage.ReusableStorage, basePort int) (string, [][]byte) {

	var baseClusterPort = basePort + 10

	// Start the cluster
	var conf = vault.CoreConfig{
		Logger: logger.Named("initializeShamir"),
	}
	var opts = vault.TestClusterOptions{
		HandlerFunc:           vaulthttp.Handler,
		BaseListenAddress:     fmt.Sprintf("127.0.0.1:%d", basePort),
		BaseClusterListenPort: baseClusterPort,
	}
	storage.Setup(&conf, &opts)
	cluster := vault.NewTestCluster(t, &conf, &opts)
	cluster.Start()
	defer func() {
		storage.Cleanup(t, cluster)
		cluster.Cleanup()
	}()

	leader := cluster.Cores[0]
	client := leader.Client

	if storage.IsRaft {
		// Join raft cluster
		testhelpers.RaftClusterJoinNodes(t, cluster)
		time.Sleep(15 * time.Second)
		verifyRaftConfiguration(t, leader)
	} else {
		// Unseal
		cluster.UnsealCores(t)
	}

	// Wait until unsealed
	testhelpers.WaitForNCoresUnsealed(t, cluster, vault.DefaultNumCores)

	// Write a secret that we will read back out later.
	_, err := client.Logical().Write(
		"secret/foo",
		map[string]interface{}{"zork": "quux"})
	if err != nil {
		t.Fatal(err)
	}

	// Seal the cluster
	cluster.EnsureCoresSealed(t)

	return cluster.RootToken, cluster.BarrierKeys
}

// reuseShamir uses a pre-populated backend storage with Shamir.
func reuseShamir(
	t *testing.T, logger hclog.Logger,
	storage teststorage.ReusableStorage, basePort int,
	rootToken string, keys [][]byte) {

	var baseClusterPort = basePort + 10

	// Start the cluster
	var conf = vault.CoreConfig{
		Logger: logger.Named("reuseShamir"),
	}
	var opts = vault.TestClusterOptions{
		HandlerFunc:           vaulthttp.Handler,
		BaseListenAddress:     fmt.Sprintf("127.0.0.1:%d", basePort),
		BaseClusterListenPort: baseClusterPort,
		SkipInit:              true,
	}
	storage.Setup(&conf, &opts)
	cluster := vault.NewTestCluster(t, &conf, &opts)
	cluster.Start()
	defer func() {
		storage.Cleanup(t, cluster)
		cluster.Cleanup()
	}()

	leader := cluster.Cores[0]
	client := leader.Client
	client.SetToken(rootToken)

	cluster.BarrierKeys = keys
	if storage.IsRaft {
		// Set hardcoded Raft address providers
		provider := testhelpers.NewHardcodedServerAddressProvider(baseClusterPort)
		testhelpers.SetRaftAddressProviders(t, cluster, provider)

		// Unseal cores
		for _, core := range cluster.Cores {
			cluster.UnsealCore(t, core)
		}
		time.Sleep(15 * time.Second)
		verifyRaftConfiguration(t, leader)
	} else {
		// Unseal
		cluster.UnsealCores(t)
	}

	// Wait until unsealed
	testhelpers.WaitForNCoresUnsealed(t, cluster, vault.DefaultNumCores)

	// Read the secret
	secret, err := client.Logical().Read("secret/foo")
	if err != nil {
		t.Fatal(err)
	}
	if diff := deep.Equal(secret.Data, map[string]interface{}{"zork": "quux"}); len(diff) > 0 {
		t.Fatal(diff)
	}

	// Seal the cluster
	cluster.EnsureCoresSealed(t)
}

func verifyRaftConfiguration(t *testing.T, core *vault.TestClusterCore) {

	backend := core.UnderlyingRawStorage.(*raft.RaftBackend)
	ctx := namespace.RootContext(context.Background())
	config, err := backend.GetConfiguration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	servers := config.Servers

	if len(servers) != vault.DefaultNumCores {
		t.Fatalf("Found %d servers, not %d", len(servers), vault.DefaultNumCores)
	}

	leaders := 0
	for i, s := range servers {
		if diff := deep.Equal(s.NodeID, fmt.Sprintf("core-%d", i)); len(diff) > 0 {
			t.Fatal(diff)
		}
		if s.Leader {
			leaders++
		}
	}

	if leaders != 1 {
		t.Fatalf("Found %d leaders, not 1", leaders)
	}
}
