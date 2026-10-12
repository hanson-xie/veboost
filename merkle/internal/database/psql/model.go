package psql

type AirdropData struct {
	ID        int64  `gorm:"primary_key" json:"id"`
	CreatedAt int64  `gorm:"not null;default:0" json:"createdAt"`
	UpdatedAt int64  `gorm:"not null;default:0" json:"updatedAt"`
	Epoch     uint64 `gorm:"not null;default:0;index:idx_epoch_user" json:"epoch"`
	Address   string `gorm:"not null;index:idx_epoch_user" json:"address"`
	Amount    string `gorm:"type:numeric(78,0);not null" json:"amount"`
	Claimed   bool   `gorm:"not null;default:false" json:"claimed"`
	// NULL means not yet computed; treated as no expiry.
	AvailableAmount *string `gorm:"type:numeric(78,0)" json:"availableAmount"`
}

type MerkleTreeData struct {
	ID            int64  `gorm:"primary_key" json:"id"`
	Epoch         uint64 `gorm:"not null;default:0;index:idx_epoch,unique" json:"epoch"`
	MarshaledTree string `gorm:"type:text;not null" json:"marshaledTree"`
}
