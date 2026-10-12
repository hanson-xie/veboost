package database

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/Bedrock-Technology/VeMerkle/internal/database/psql"
	"github.com/sirupsen/logrus"
)

// GetMaxEpoch retrieves the maximum epoch from airdrop_data table
// Returns 0 if table is empty
func GetMaxEpoch() (uint64, error) {
	db, err := GetDBConnection("postgres")
	if err != nil {
		return 0, err
	}

	var maxEpoch struct {
		MaxEpoch uint64
	}

	result := db.Model(&psql.AirdropData{}).
		Select("COALESCE(MAX(epoch), 0) as max_epoch").
		Scan(&maxEpoch)

	if result.Error != nil {
		return 0, result.Error
	}

	return maxEpoch.MaxEpoch, nil
}

// CheckEpochValidity validates if the given epoch is valid for new airdrop
// Returns true if:
// 1. Database is empty and epoch > 0 (first airdrop)
// 2. epoch = max_epoch + 1 (sequential airdrop)
func CheckEpochValidity(epoch uint64) (bool, error) {
	maxEpoch, err := GetMaxEpoch()
	if err != nil {
		return false, err
	}

	if maxEpoch == 0 {
		return epoch == 1, nil
	}

	return epoch == maxEpoch+1, nil
}

func CheckCurEpochValidity(epoch uint64) (bool, error) {
	maxEpoch, err := GetMaxEpoch()
	if err != nil {
		return false, err
	}
	if maxEpoch == 0 {
		return false, nil
	}
	return epoch == maxEpoch, nil
}

// GetAirdropByEpoch retrieves airdrop data for specific epoch
func GetAirdropByEpoch(epoch uint64) (*psql.AirdropData, error) {
	db, err := GetDBConnection("postgres")
	if err != nil {
		return nil, err
	}

	var airdrop psql.AirdropData
	err = db.Where("epoch = ?", epoch).First(&airdrop).Error
	if err != nil {
		return nil, err
	}
	return &airdrop, nil
}

// BatchCreateAirdropData creates multiple airdrop records in database efficiently
func BatchCreateAirdropData(records []*psql.AirdropData) error {
	if len(records) == 0 {
		return nil
	}

	db, err := GetDBConnection("postgres")
	if err != nil {
		return err
	}

	// Use transaction for batch insertion
	tx := db.Begin()
	if tx.Error != nil {
		return tx.Error
	}

	// Set larger batch size for better performance
	batchSize := 1000
	for i := 0; i < len(records); i += batchSize {
		end := i + batchSize
		if end > len(records) {
			end = len(records)
		}
		logrus.WithFields(logrus.Fields{
			"batch_start": i,
			"batch_end":   end,
			"batch_size":  end - i,
		}).Info("Processing batch")
		if err := tx.CreateInBatches(records[i:end], batchSize).Error; err != nil {
			tx.Rollback()
			return err
		}
		logrus.WithFields(logrus.Fields{
			"batch_start": i,
			"batch_end":   end,
		}).Info("Batch processed successfully")
	}

	return tx.Commit().Error
}

func GetUsersByEpoch(epoch uint64) ([]string, error) {
	db, err := GetDBConnection("postgres")
	if err != nil {
		return nil, err
	}

	var users []string
	err = db.Model(&psql.AirdropData{}).Where("epoch = ?", epoch).Pluck("address", &users).Error
	if err != nil {
		return nil, err
	}
	return users, nil
}

func UpdateClaimedStatus(epoch uint64, users []string, claimedStatus []bool) error {
	db, err := GetDBConnection("postgres")
	if err != nil {
		return err
	}

	tx := db.Begin()
	if tx.Error != nil {
		return tx.Error
	}

	for i, user := range users {
		err := tx.Model(&psql.AirdropData{}).Where("epoch = ? AND address = ?", epoch, user).Update("claimed", claimedStatus[i]).Error
		if err != nil {
			tx.Rollback()
			return err
		}
	}

	return tx.Commit().Error
}

// GetAllAirdropDataByEpoch retrieves all airdrop data for a specific epoch
func GetAllAirdropDataByEpoch(epoch uint64) ([]*psql.AirdropData, error) {
	db, err := GetDBConnection("postgres")
	if err != nil {
		return nil, err
	}

	var records []*psql.AirdropData
	err = db.Where("epoch = ?", epoch).Find(&records).Error
	if err != nil {
		return nil, err
	}
	return records, nil
}

// GetClaimedAirdropDataByEpoch retrieves airdrop data for a specific epoch based on claimed status
func GetClaimedAirdropDataByEpoch(epoch uint64, claimed bool) ([]*psql.AirdropData, error) {
	db, err := GetDBConnection("postgres")
	if err != nil {
		return nil, err
	}

	var records []*psql.AirdropData
	err = db.Where("epoch = ? AND claimed = ?", epoch, claimed).Find(&records).Error
	if err != nil {
		return nil, err
	}
	return records, nil
}

// GetAvailableAirdropDataByEpoch returns unclaimed rows of the epoch with Amount set to the available amount.
func GetAvailableAirdropDataByEpoch(epoch uint64) ([]*psql.AirdropData, error) {
	db, err := GetDBConnection("postgres")
	if err != nil {
		return nil, err
	}

	var records []*psql.AirdropData
	err = db.Select("id, epoch, address, claimed, COALESCE(available_amount, amount) AS amount").
		Where("epoch = ? AND claimed = ? AND COALESCE(available_amount, amount) > 0", epoch, false).
		Find(&records).Error
	if err != nil {
		return nil, err
	}
	return records, nil
}

// RefreshAvailableAmount computes available_amount for unclaimed rows of the epoch,
// expiring balances that have not been claimed within the last window epochs.
func RefreshAvailableAmount(epoch, window uint64) error {
	db, err := GetDBConnection("postgres")
	if err != nil {
		return err
	}

	var current []*psql.AirdropData
	if err := db.Where("epoch = ? AND claimed = ?", epoch, false).Find(&current).Error; err != nil {
		return err
	}
	if len(current) == 0 {
		return nil
	}

	available := make(map[int64]string, len(current))
	if window == 0 || epoch <= window {
		for _, r := range current {
			available[r.ID] = r.Amount
		}
	} else {
		start := epoch - window
		var history []*psql.AirdropData
		if err := db.Where("epoch >= ? AND epoch < ?", start, epoch).Find(&history).Error; err != nil {
			return err
		}

		byAddr := make(map[string]map[uint64]*psql.AirdropData)
		for _, r := range history {
			addr := strings.ToLower(r.Address)
			if byAddr[addr] == nil {
				byAddr[addr] = make(map[uint64]*psql.AirdropData)
			}
			byAddr[addr][r.Epoch] = r
		}

		for _, r := range current {
			v, err := computeAvailableAmount(r, byAddr[strings.ToLower(r.Address)], start, epoch)
			if err != nil {
				return err
			}
			available[r.ID] = v
		}
	}

	tx := db.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	for id, v := range available {
		if err := tx.Model(&psql.AirdropData{}).Where("id = ?", id).Update("available_amount", v).Error; err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit().Error
}

// computeAvailableAmount applies the expiry rule; history holds the address rows in [start, epoch).
func computeAvailableAmount(cur *psql.AirdropData, history map[uint64]*psql.AirdropData, start, epoch uint64) (string, error) {
	for k := start; k < epoch; k++ {
		// A claim or missing row means the balance was reset, so nothing older remains.
		if r := history[k]; r == nil || r.Claimed {
			return cur.Amount, nil
		}
	}

	amount, err := parseAmount(cur.Amount)
	if err != nil {
		return "", err
	}
	base, err := parseAmount(history[start].Amount)
	if err != nil {
		return "", err
	}
	result := new(big.Int).Sub(amount, base)

	// Add back amounts already expired in [start, epoch) so they are not deducted twice.
	for k := start; k < epoch; k++ {
		r := history[k]
		if r.AvailableAmount == nil {
			continue
		}
		a, err := parseAmount(r.Amount)
		if err != nil {
			return "", err
		}
		av, err := parseAmount(*r.AvailableAmount)
		if err != nil {
			return "", err
		}
		result.Add(result, a.Sub(a, av))
	}

	if result.Sign() < 0 {
		result.SetInt64(0)
	}
	return result.String(), nil
}

func parseAmount(s string) (*big.Int, error) {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil, fmt.Errorf("invalid amount format: %s", s)
	}
	return v, nil
}

// DeleteAirdropDataByEpoch deletes all airdrop data for a specific epoch
func DeleteAirdropDataByEpoch(epoch uint64) error {
	db, err := GetDBConnection("postgres")
	if err != nil {
		return err
	}

	// Use transaction for batch deletion
	tx := db.Begin()
	if tx.Error != nil {
		return tx.Error
	}

	// Delete all records for the specified epoch
	if err := tx.Where("epoch = ?", epoch).Delete(&psql.AirdropData{}).Error; err != nil {
		tx.Rollback()
		return err
	}

	return tx.Commit().Error
}

// CheckEpochExists checks if any records exist for the given epoch
// Returns true if records exist, false otherwise
func CheckEpochExists(epoch uint64) (bool, error) {
	db, err := GetDBConnection("postgres")
	if err != nil {
		return false, err
	}

	var count int64
	err = db.Model(&psql.AirdropData{}).
		Where("epoch = ?", epoch).
		Count(&count).Error

	if err != nil {
		return false, err
	}

	return count > 0, nil
}

// CreateMerkleTreeMarshaledData creates a new Merkle tree record in the database
func CreateMerkleTreeMarshaledData(epoch uint64, marshaledData []byte) error {
	db, err := GetDBConnection("postgres")
	if err != nil {
		return err
	}

	merkleTree := psql.MerkleTreeData{
		Epoch:         epoch,
		MarshaledTree: string(marshaledData),
	}

	return db.Create(&merkleTree).Error
}

// GetMerkleTreeMarshaledData retrieves a marshaled Merkle tree from the database by epoch
func GetMerkleTreeMarshaledData(epoch uint64) ([]byte, error) {
	db, err := GetDBConnection("postgres")
	if err != nil {
		return nil, err
	}

	var merkleTree psql.MerkleTreeData
	err = db.Where("epoch = ?", epoch).First(&merkleTree).Error
	if err != nil {
		return nil, err
	}

	return []byte(merkleTree.MarshaledTree), nil
}

// DeleteMerkleTreeDataByEpoch deletes a marshaled Merkle tree from the database by epoch
func DeleteMerkleTreeDataByEpoch(epoch uint64) error {
	db, err := GetDBConnection("postgres")
	if err != nil {
		return err
	}

	// Use transaction for deletion
	tx := db.Begin()
	if tx.Error != nil {
		return tx.Error
	}

	// Delete the record for the specified epoch
	if err := tx.Where("epoch = ?", epoch).Delete(&psql.MerkleTreeData{}).Error; err != nil {
		tx.Rollback()
		return err
	}

	return tx.Commit().Error
}

// ImportAirdropWithMerkleTree creates multiple airdrop records and a Merkle tree record in the database within a single transaction.
func ImportAirdropWithMerkleTree(epoch uint64, records []*psql.AirdropData, marshaledData []byte) error {
	db, err := GetDBConnection("postgres")
	if err != nil {
		return err
	}

	if len(records) == 0 {
		return nil
	}

	tx := db.Begin()
	if tx.Error != nil {
		return tx.Error
	}

	// Batch insert airdrop data
	batchSize := 1000
	for i := 0; i < len(records); i += batchSize {
		end := min(i+batchSize, len(records))
		logrus.WithFields(logrus.Fields{
			"batch_start": i,
			"batch_end":   end,
			"batch_size":  end - i,
		}).Info("Processing batch")

		if err := tx.CreateInBatches(records[i:end], batchSize).Error; err != nil {
			tx.Rollback()
			return err
		}
		logrus.WithFields(logrus.Fields{
			"batch_start": i,
			"batch_end":   end,
		}).Info("Batch processed successfully")
	}

	// Create Merkle tree record
	merkleTree := psql.MerkleTreeData{
		Epoch:         epoch,
		MarshaledTree: string(marshaledData),
	}
	if err := tx.Create(&merkleTree).Error; err != nil {
		tx.Rollback()
		return err
	}

	return tx.Commit().Error
}

// DeleteAirdropAndMerkleTreeByEpoch deletes all airdrop data and the Merkle tree for a specific epoch within a single transaction.
func DeleteAirdropAndMerkleTreeByEpoch(epoch uint64) error {
	db, err := GetDBConnection("postgres")
	if err != nil {
		return err
	}

	tx := db.Begin()
	if tx.Error != nil {
		return tx.Error
	}

	// Delete airdrop data
	if err := tx.Where("epoch = ?", epoch).Delete(&psql.AirdropData{}).Error; err != nil {
		tx.Rollback()
		return err
	}

	// Delete Merkle tree data
	if err := tx.Where("epoch = ?", epoch).Delete(&psql.MerkleTreeData{}).Error; err != nil {
		tx.Rollback()
		return err
	}

	return tx.Commit().Error
}
