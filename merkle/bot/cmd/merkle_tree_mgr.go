package cmd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"sync"
	"time"

	"github.com/Bedrock-Technology/VeMerkle/internal/contracts"
	"github.com/Bedrock-Technology/VeMerkle/internal/database"
	"github.com/Bedrock-Technology/VeMerkle/internal/database/psql"
	merkletree "github.com/FantasyJony/openzeppelin-merkle-tree-go/standard_merkle_tree"
)

var MerkleTreeManager *merkleTreeManager

func MerkleTreeManagerInit() {
	mgr := &merkleTreeManager{
		trees: make(map[uint64]*MerkleTree),
	}

	// Get the latest epoch from the database
	maxEpoch, err := database.GetMaxEpoch()
	if err != nil {
		slog.Error("Failed to get max epoch from database")
		panic(err)
	}

	if maxEpoch == 0 {
		slog.Info("No Merkle tree found in database, initializing empty manager")
		MerkleTreeManager = mgr
		return
	}

	// If maxEpoch is greater than 0, it means there's a Merkle tree to load
	slog.Info("Loading latest Merkle tree into memory", slog.Uint64("epoch", maxEpoch))
	// Use the Get method to load the Merkle tree, which handles both
	// database retrieval and in-memory caching.
	if _, err := mgr.Get(maxEpoch); err != nil {
		slog.Error("Failed to load Merkle tree for the latest epoch", slog.Uint64("epoch", maxEpoch), slog.Any("error", err))
		panic(err)
	}

	mgr.mu.Lock()
	defer mgr.mu.Unlock()

	mgr.maxEpoch = maxEpoch
	MerkleTreeManager = mgr
	slog.Info("[MerkleTreeManager] Initialized successfully", slog.Uint64("max_epoch", maxEpoch))
}

// MerkleTreeManager is a type alias for a map that manages MerkleTree instances by epoch.
type merkleTreeManager struct {
	trees    map[uint64]*MerkleTree
	maxEpoch uint64
	mu       sync.Mutex
}

// MerkleTree holds the standard Merkle tree, along with maps for quick address and amount lookups.
type MerkleTree struct {
	Epoch   uint64
	Tree    *merkletree.StandardTree
	Address map[string]int
	Amount  map[string]*big.Int
}

// MarshalJSON customizes the JSON serialization for the MerkleTree struct.
// It marshals the underlying StandardTree and combines it with the address and amount maps
// into a single JSON object for storage.
func (mt *MerkleTree) MarshalJSON() ([]byte, error) {
	treeData, err := mt.Tree.TreeMarshal()
	if err != nil {
		return nil, err
	}

	return json.Marshal(&struct {
		Tree    []byte              `json:"tree"`
		Address map[string]int      `json:"address"`
		Amount  map[string]*big.Int `json:"amount"`
	}{
		Tree:    treeData,
		Address: mt.Address,
		Amount:  mt.Amount,
	})
}

// UnmarshalJSON customizes the JSON deserialization for the MerkleTree struct.
// It parses the JSON data, reconstructs the StandardTree from its marshaled format,
// and populates the address and amount maps.
func (mt *MerkleTree) UnmarshalJSON(data []byte) error {
	aux := &struct {
		Tree    []byte              `json:"tree"`
		Address map[string]int      `json:"address"`
		Amount  map[string]*big.Int `json:"amount"`
	}{}

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	tree, err := merkletree.TreeUnmarshal(aux.Tree)
	if err != nil {
		return err
	}

	mt.Tree = tree
	mt.Address = aux.Address
	mt.Amount = aux.Amount
	return nil
}

// Import handles the process of importing a new Merkle tree for a given epoch.
// It performs several validation checks:
// 1. Verifies with the smart contract that the epoch is the expected next epoch.
// 2. Verifies with the database that the epoch is new and does not already exist.
// If validation passes, it prepares the airdrop records, marshals the Merkle tree,
// and saves both to the database in a single transaction. Finally, it stores the
// Merkle tree in the in-memory manager.
func (m *merkleTreeManager) Import(merkleTree *MerkleTree, epoch uint64) error {
	// check epoch is equal to current epoch + 1
	proxy := contracts.GetProxy()
	valid, err := proxy.CheckEpochValidity(epoch)
	if err != nil {
		return fmt.Errorf("failed to check epoch validity in contract: %v", err)
	}

	if !valid {
		return fmt.Errorf("invalid epoch, expected next epoch")
	}

	// check epoch is not record in db
	valid, err = database.CheckEpochValidity(epoch)
	if err != nil {
		return fmt.Errorf("failed to check epoch validity in database: %v", err)
	}

	if !valid {
		return fmt.Errorf("epoch already exists in database")
	}

	airdropRecords := make([]*psql.AirdropData, 0, len(merkleTree.Address))

	for addr := range merkleTree.Address {
		airdropRecords = append(airdropRecords, &psql.AirdropData{
			Epoch:     epoch,
			Address:   addr,
			Amount:    merkleTree.Amount[addr].String(),
			Claimed:   false,
			CreatedAt: time.Now().Unix(),
		})
	}

	merkleTree.Epoch = epoch

	// Marshal the Merkle tree
	marshaledData, err := json.Marshal(merkleTree)
	if err != nil {
		return fmt.Errorf("failed to marshal merkle tree: %v", err)
	}

	// Use the new transaction-safe function to import data
	if err := database.ImportAirdropWithMerkleTree(epoch, airdropRecords, marshaledData); err != nil {
		return fmt.Errorf("failed to save airdrop and merkle tree data: %v", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.trees[epoch] = merkleTree
	slog.Info("[MerkleTreeManager] Import", slog.Uint64("max_epoch", epoch))

	m.maxEpoch = epoch
	return nil
}

// Update replaces the in-memory Merkle tree for a given epoch.
// It validates that the provided epoch matches the current epoch on-chain and in the database.
// This function only affects the in-memory representation and does not persist any changes to the database.
func (m *merkleTreeManager) Update(merkleTree *MerkleTree, epoch uint64) error {
	// check epoch is equal to current epoch
	proxy := contracts.GetProxy()
	valid, err := proxy.CheckCurEpochValidity(epoch)
	if err != nil {
		return fmt.Errorf("failed to check epoch validity in contract: %v", err)
	}

	if !valid {
		return fmt.Errorf("invalid epoch, expected current epoch")
	}

	// check epoch is not record in db
	valid, err = database.CheckCurEpochValidity(epoch)
	if err != nil {
		return fmt.Errorf("failed to check epoch validity in database: %v", err)
	}

	if !valid {
		return fmt.Errorf("epoch does not exist in database")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.trees[epoch] = merkleTree
	return nil
}

// Delete removes a Merkle tree and its associated airdrop data for a specific epoch.
// It first validates that the epoch can be reverted (currentEpoch + 1 on-chain).
// It uses a transaction-safe database function to delete data from both the airdrop
// and Merkle tree tables, ensuring atomicity. After successful database deletion,
// it removes the Merkle tree from the in-memory manager.
func (m *merkleTreeManager) Delete(epoch uint64) error {
	// Check if the epoch is valid for reversion (currentEpoch + 1)
	proxy := contracts.GetProxy()
	valid, err := proxy.CheckEpochValidity(epoch)
	if err != nil {
		return fmt.Errorf("failed to check epoch validity on-chain for epoch %d: %v", epoch, err)
	}

	if !valid {
		return fmt.Errorf("revert failed: Epoch %d is not the next epoch to be processed. You can only revert an epoch that has not been set on-chain yet", epoch)
	}

	// Use the transaction-safe function to delete data
	if err := database.DeleteAirdropAndMerkleTreeByEpoch(epoch); err != nil {
		return fmt.Errorf("failed to delete airdrop and merkle tree data from database: %v", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Delete from memory
	delete(m.trees, epoch)
	slog.Info("[MerkleTreeManager] Delete", slog.Uint64("max_epoch", epoch-1))

	m.maxEpoch = epoch - 1
	return nil
}

// Get retrieves a Merkle tree for a given epoch.
// It first checks for the tree in the in-memory cache. If not found, it queries the
// database for the marshaled tree data, unmarshals it, and stores it in memory
// for future access before returning it.
func (m *merkleTreeManager) Get(epoch uint64) (*MerkleTree, error) {
	m.mu.Lock()
	tree, found := m.trees[epoch]
	m.mu.Unlock()
	if found {
		return tree, nil
	}

	// If not in memory, try to load from database
	marshaledData, err := database.GetMerkleTreeMarshaledData(epoch)
	if err != nil {
		return nil, fmt.Errorf("merkle tree for epoch %d not found in database: %v", epoch, err)
	}

	var merkleTree MerkleTree
	if err := json.Unmarshal(marshaledData, &merkleTree); err != nil {
		return nil, fmt.Errorf("failed to unmarshal merkle tree: %v", err)
	}

	merkleTree.Epoch = epoch

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check again in case another goroutine loaded the tree while we were working
	if existingTree, ok := m.trees[epoch]; ok {
		return existingTree, nil
	}

	// Store in memory for future access
	m.trees[epoch] = &merkleTree

	return &merkleTree, nil
}

// GetLatestMerkleTree retrieves the Merkle tree for the highest epoch.
func (m *merkleTreeManager) GetLatestMerkleTree() (*MerkleTree, error) {
	m.mu.Lock()
	maxEpoch := m.maxEpoch
	m.mu.Unlock()

	if maxEpoch == 0 {
		return nil, fmt.Errorf("no merkle tree found in manager")
	}
	return m.Get(maxEpoch)
}
