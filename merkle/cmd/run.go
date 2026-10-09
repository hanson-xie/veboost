package cmd

import (
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Bedrock-Technology/VeMerkle/bot"
	botCmd "github.com/Bedrock-Technology/VeMerkle/bot/cmd"
	botConfig "github.com/Bedrock-Technology/VeMerkle/bot/config"
	"github.com/Bedrock-Technology/VeMerkle/internal/config"
	"github.com/Bedrock-Technology/VeMerkle/internal/contracts"
	"github.com/Bedrock-Technology/VeMerkle/internal/database"
	"github.com/Bedrock-Technology/VeMerkle/internal/database/psql"
	"github.com/Bedrock-Technology/VeMerkle/internal/logger"
	"github.com/Bedrock-Technology/VeMerkle/internal/proto"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	smt "github.com/FantasyJony/openzeppelin-merkle-tree-go/standard_merkle_tree"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	sloglogrus "github.com/samber/slog-logrus/v2"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
)

var (
	port     int
	merkleDB *merkleTree
)

type merkleTree struct {
	epoch     uint64
	tree      *smt.StandardTree
	addresses map[string]int
	amounts   map[string]*big.Int
}

// @title Airdrop Merkle API
// @version 1.0
// @description API for verifying Merkle proofs of airdrop data
// @host localhost:8080
// @BasePath /api/v1
var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Start the Merkle proof verification API server",
	Long: `Start a HTTP server that provides API endpoints for Merkle proof verification.
    The Merkle tree data should be imported via the /merkle/import API endpoint.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Load configuration
		configPath := filepath.Join("configs", "airdrop.yaml")
		err := config.InitConfig(configPath)
		if err != nil {
			return fmt.Errorf("failed to load config: %v", err)
		}

		// Initialize logger with loaded configuration
		if err := logger.InitLogger(config.GetConfig().Logger); err != nil {
			return fmt.Errorf("failed to initialize logger: %v", err)
		}

		// Initialize contracts proxy
		err = contracts.InitProxy()
		if err != nil {
			return fmt.Errorf("failed to initialize contracts proxy: %v", err)
		}

		if err := godotenv.Load(); err != nil {
			return fmt.Errorf("failed to load .env file: %v", err)
		}

		logger := slog.New(sloglogrus.Option{Level: slog.LevelDebug, Logger: logrus.StandardLogger()}.NewLogrusHandler())
		slog.SetDefault(logger)

		// Register bot commands
		bot.RegisterSlackBotCommands()

		// Initialize database
		database.InitPostgres()

		// Initialize Merkle tree manager
		botCmd.MerkleTreeManagerInit()

		// Setup Gin router
		gin.SetMode(gin.ReleaseMode)
		r := gin.New()
		r.Use(gin.Recovery())
		r.Use(loggerMiddleware())
		r.Use(CORSMiddleware())
		setupRouter(r)
		setupSwagger(r, config.GetConfig().DocAuth)

		// Start server
		addr := fmt.Sprintf(":%d", port)
		logrus.WithField("port", port).Info("Server starting")
		return r.Run(addr)
	},
}

func setupRouter(r *gin.Engine) {
	api := r.Group("/api/v1")
	{
		// API endpoints
		api.POST("/merkle/proof", getMerkleProof)
		api.POST("/merkle/import", importMerkleTreeHandler)
		api.GET("/merkle/root", getMerkleRoot)
		api.POST("/merkle/claimed", updateClaimedAirdropData)
		api.POST("/merkle/import_airdrop", importAirdropHandler)
		api.POST("/merkle/export_airdrop", exportAirdropHandler)
		api.POST("/merkle/delete_airdrop", deleteAirdropHandler)
		api.POST("/slack/events", slackBotEventHandler)
	}
}

func setupSwagger(e *gin.Engine, docAuth map[string]string) {
	auth := gin.Accounts(docAuth)
	e.GET("/docs/*any", gin.BasicAuth(auth), ginSwagger.WrapHandler(swaggerFiles.Handler))
	e.StaticFile("/swagger/doc.json", "./docs/swagger.json")
}

func init() {
	rootCmd.AddCommand(runCmd)
	runCmd.Flags().IntVar(&port, "port", 8080, "Port to run the server on")
}

func CORSMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS, PUT, DELETE")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Origin, Content-Type, Authorization")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// loggerMiddleware creates a Gin middleware for logging HTTP requests
func loggerMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Process request
		c.Next()

		// Log request details
		logrus.WithFields(logrus.Fields{
			"status":     c.Writer.Status(),
			"method":     c.Request.Method,
			"path":       c.Request.URL.Path,
			"ip":         c.ClientIP(),
			"user_agent": c.Request.UserAgent(),
			"latency":    c.Writer.Header().Get("X-Response-Time"),
		}).Info("HTTP request")
	}
}

// getMerkleProof handles the Merkle proof API endpoint
// @Summary Get Merkle proof for address
// @Description Get Merkle proof, amount and root hash for a given address
// @Tags merkle
// @Accept json
// @Produce json
// @Param address body string true "Ethereum address"
// @Success 200 {object} map[string]interface{} "Returns address, amount, proof array and root hash"
// @Failure 404 {object} map[string]string "Returns error message when address not found"
// @Router /merkle/proof [post]
func getMerkleProof(c *gin.Context) {
	var request struct {
		Address string `json:"address"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		logrus.WithError(err).Error("Failed to bind JSON")
		proto.ErrorMsg(c, "failed to bind JSON")
		return
	}

	address := strings.ToLower(request.Address)

	merkleDB, err := botCmd.MerkleTreeManager.GetLatestMerkleTree()
	if err != nil {
		logrus.WithError(err).Error("Failed to get latest Merkle tree")
		proto.ErrorMsg(c, "failed to get latest Merkle tree")
		return
	}

	// Check if address exists
	index, exists := merkleDB.Address[address]
	if !exists {
		logrus.WithField("address", request.Address).Warn("Address not found")
		proto.ErrorMsg(c, "address not found")
		return
	}
	logrus.WithFields(logrus.Fields{
		"query address": address,
		"in array indx": index,
	}).Info("Reading merkle db")

	// Get amount and proof
	amount := merkleDB.Amount[address]
	leaf := []interface{}{
		smt.SolAddress(address),
		smt.SolNumber(amount.String()),
	}

	proof, err := merkleDB.Tree.GetProof(leaf)
	if err != nil {
		logrus.WithError(err).Error("Failed to get Merkle proof")
		proto.ErrorMsg(c, "failed to get Merkle proof")
		return
	}

	// Verify the proof
	verify, err := merkleDB.Tree.Verify(proof, leaf)
	if err != nil {
		logrus.WithError(err).Error("Failed to verify Merkle proof")
		proto.ErrorMsg(c, "failed to verify Merkle proof")
		return
	}
	if !verify {
		logrus.WithFields(logrus.Fields{
			"address": address,
			"amount":  amount.String(),
		}).Error("Invalid Merkle proof generated")
		proto.ErrorMsg(c, "invalid merkle proof")
		return
	}

	// Convert proof to hex strings
	hexProof := make([]string, len(proof))
	for i, p := range proof {
		hexProof[i] = "0x" + hex.EncodeToString(p)
	}

	logrus.WithFields(logrus.Fields{
		"address": address,
		"amount":  amount.String(),
	}).Info("Generated Merkle proof")

	treeRoot := hexutil.Encode(merkleDB.Tree.GetRoot())
	proto.SuccessMsg(c, http.StatusOK, "Merkle proof generated successfully", gin.H{
		"epoch":   merkleDB.Epoch,
		"address": address,
		"amount":  amount.String(),
		"proof":   hexProof,
		"root":    treeRoot,
	})
}

// @Summary Get Merkle tree root
// @Description Retrieve the root hash of the Merkle tree
// @Tags merkle
// @Produce json
// @Success 200 {object} map[string]string "Returns the root hash of the Merkle tree"
// @Failure 404 {object} map[string]string "Returns error message when Merkle tree is not initialized"
// @Router /merkle/root [get]
func getMerkleRoot(c *gin.Context) {
	merkleDB, err := botCmd.MerkleTreeManager.GetLatestMerkleTree()
	if err != nil {
		logrus.WithError(err).Error("Failed to get latest Merkle tree")
		proto.ErrorMsg(c, "failed to get latest Merkle tree")
		return
	}

	if merkleDB == nil || merkleDB.Tree == nil {
		proto.ErrorMsg(c, "Merkle tree not initialized")
		return
	}

	treeRoot := hexutil.Encode(merkleDB.Tree.GetRoot())
	logrus.WithField("root", treeRoot).Info("Retrieved Merkle tree root")
	proto.SuccessMsg(c, http.StatusOK, "Merkle tree root retrieved successfully", gin.H{
		"epoch": merkleDB.Epoch,
		"root":  treeRoot,
	})
}

// importMerkleTree reads a CSV file and updates the merkleDB with new data
func importOnlyMerkleTree(csvFile string, epoch uint64) error {
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
	// read csv file
	file, err := os.Open(csvFile)
	if err != nil {
		return fmt.Errorf("failed to open CSV file: %v", err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	records, err := reader.ReadAll()
	if err != nil {
		return fmt.Errorf("failed to read CSV file: %v", err)
	}

	logrus.WithField("records length", len(records)).Info("Reading CSV file")
	// Skip header row
	if len(records) < 2 {
		return fmt.Errorf("CSV file is empty or contains only header")
	}
	records = records[1:]

	// Prepare new data for Merkle tree
	addresses := make(map[string]int)
	amounts := make(map[string]*big.Int)
	values := [][]interface{}{}

	for i, record := range records {
		if len(record) != 2 {
			return fmt.Errorf("invalid CSV record format at line %d", i+2)
		}
		address := strings.ToLower(record[0])
		amount, ok := new(big.Int).SetString(record[1], 10)
		if !ok {
			return fmt.Errorf("invalid amount format at line %d", i+2)
		}

		addresses[address] = i
		amounts[address] = amount
		values = append(values, []interface{}{
			smt.SolAddress(address),
			smt.SolNumber(record[1]),
		})
	}

	// Generate Merkle tree
	leafEncodings := []string{
		smt.SOL_ADDRESS,
		smt.SOL_UINT256,
	}
	if values == nil {
		return fmt.Errorf("values are nil")
	}

	tree, err := smt.Of(values, leafEncodings)
	if err != nil {
		return fmt.Errorf("failed to create merkle tree: %v", err)
	}

	merkleDB = &merkleTree{
		epoch:     epoch,
		tree:      tree,
		addresses: addresses,
		amounts:   amounts,
	}

	return nil
}

// importMerkleTree reads a CSV file and updates the merkleDB with new data and records in the database
func importMerkleTree(csvFile string, epoch uint64) error {
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
	// read csv file
	file, err := os.Open(csvFile)
	if err != nil {
		return fmt.Errorf("failed to open CSV file: %v", err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	records, err := reader.ReadAll()
	if err != nil {
		return fmt.Errorf("failed to read CSV file: %v", err)
	}

	logrus.WithField("records length", len(records)).Info("Reading CSV file")
	// Skip header row
	if len(records) < 2 {
		return fmt.Errorf("CSV file is empty or contains only header")
	}
	records = records[1:]

	// Prepare new data for Merkle tree
	addresses := make(map[string]int)
	amounts := make(map[string]*big.Int)
	values := [][]interface{}{}
	// Prepare batch records for database insertion
	airdropRecords := make([]*psql.AirdropData, 0, len(records))

	for i, record := range records {
		if len(record) != 2 {
			return fmt.Errorf("invalid CSV record format at line %d", i+2)
		}
		address := strings.ToLower(record[0])
		amount, ok := new(big.Int).SetString(record[1], 10)
		if !ok {
			return fmt.Errorf("invalid amount format at line %d", i+2)
		}

		addresses[address] = i
		amounts[address] = amount
		values = append(values, []interface{}{
			smt.SolAddress(address),
			smt.SolNumber(record[1]),
		})

		// Save airdrop data for batch insertion
		airdropRecords = append(airdropRecords, &psql.AirdropData{
			Epoch:     epoch,
			Address:   address,
			Amount:    amount.String(),
			Claimed:   false,
			CreatedAt: time.Now().Unix(),
		})
	}

	// Generate Merkle tree
	leafEncodings := []string{
		smt.SOL_ADDRESS,
		smt.SOL_UINT256,
	}
	if values == nil {
		return fmt.Errorf("values are nil")
	}

	tree, err := smt.Of(values, leafEncodings)
	if err != nil {
		return fmt.Errorf("failed to create merkle tree: %v", err)
	}

	// Batch insert into database
	if err := database.BatchCreateAirdropData(airdropRecords); err != nil {
		return fmt.Errorf("failed to save airdrop data: %v", err)
	}

	merkleDB = &merkleTree{
		epoch:     epoch,
		tree:      tree,
		addresses: addresses,
		amounts:   amounts,
	}

	return nil
}

func importMerkleTreeHandler(c *gin.Context) {
	var request struct {
		CsvFile string `json:"csvFile"`
		Epoch   uint64 `json:"epoch"`
		Persist bool   `json:"persist"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		logrus.WithError(err).Error("Failed to bind JSON")
		proto.ErrorMsg(c, "failed to bind JSON")
		return
	}

	// Import Merkle tree data
	if request.Persist {
		logrus.Info("Importing Merkle tree with database records")
		if err := importMerkleTree(request.CsvFile, request.Epoch); err != nil {
			logrus.WithError(err).Error("Failed to import Merkle tree")
			proto.ErrorMsg(c, err.Error())
			return
		}
	} else {
		logrus.Info("Importing Merkle tree without database records")
		if err := importOnlyMerkleTree(request.CsvFile, request.Epoch); err != nil {
			logrus.WithError(err).Error("Failed to import Merkle tree")
			proto.ErrorMsg(c, err.Error())
			return
		}
	}

	proto.SuccessMsg(c, http.StatusOK, "Merkle tree imported successfully", nil)
}

func updateClaimedAirdropData(c *gin.Context) {
	var request struct {
		Epoch uint64 `json:"epoch"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		logrus.WithError(err).Error("Failed to bind JSON")
		proto.ErrorMsg(c, "failed to bind JSON")
		return
	}

	// Step 1: Check if the current epoch's airdrop is active
	proxy := contracts.GetProxy()
	isActive, err := proxy.IsCurrentEpochActive()
	if err != nil {
		logrus.WithError(err).Error("Failed to check if current epoch is active")
		proto.ErrorMsg(c, "failed to check if current epoch is active")
		return
	}
	if isActive {
		proto.ErrorMsg(c, "current epoch is still active")
		return
	}

	// Step 2: Check if the provided epoch matches the current epoch
	currentEpoch, err := proxy.GetCurrentEpoch()
	if err != nil {
		logrus.WithError(err).Error("Failed to get current epoch")
		proto.ErrorMsg(c, "failed to get current epoch")
		return
	}
	if request.Epoch != currentEpoch {
		proto.ErrorMsg(c, "provided epoch does not match current epoch")
		return
	}

	// Step 3: Retrieve all users for the current epoch from the database
	userStrings, err := database.GetUsersByEpoch(request.Epoch)
	if err != nil {
		logrus.WithError(err).Error("Failed to retrieve users from database")
		proto.ErrorMsg(c, "failed to retrieve users from database")
		return
	}

	// Convert user strings to common.Address
	users := make([]common.Address, len(userStrings))
	for i, user := range userStrings {
		users[i] = common.HexToAddress(user)
	}

	// Step 4: Check claim status using the contract and update the database in batches
	batchSize := 1000
	for i := 0; i < len(users); i += batchSize {
		end := i + batchSize
		if end > len(users) {
			end = len(users)
		}

		logrus.WithFields(logrus.Fields{
			"batch_start": i,
			"batch_end":   end,
			"batch_size":  end - i,
		}).Info("Processing batch")
		claimedStatus, err := proxy.HasUsersClaimed(big.NewInt(int64(request.Epoch)), users[i:end])
		if err != nil {
			logrus.WithError(err).Error("Failed to check claim status")
			proto.ErrorMsg(c, "failed to check claim status")
			return
		}

		if err := database.UpdateClaimedStatus(request.Epoch, userStrings[i:end], claimedStatus); err != nil {
			logrus.WithError(err).Error("Failed to update claimed status in database")
			proto.ErrorMsg(c, "failed to update claimed status in database")
			return
		}
		logrus.WithFields(logrus.Fields{
			"batch_start": i,
			"batch_end":   end,
		}).Info("Batch processed successfully")
	}

	proto.SuccessMsg(c, http.StatusOK, "Claimed status updated successfully", nil)
}

func exportAirdropHandler(c *gin.Context) {
	var request struct {
		Epoch   uint64 `json:"epoch"`
		CsvFile string `json:"csvFile"`
		Claimed int    `json:"claimed"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		logrus.WithError(err).Error("Failed to bind JSON")
		proto.ErrorMsg(c, "failed to bind JSON")
		return
	}

	// Step 1: Check if epoch exists in the database (optimized)
	exists, err := database.CheckEpochExists(request.Epoch)
	if err != nil {
		logrus.WithError(err).Error("Failed to check if epoch exists")
		proto.ErrorMsg(c, "failed to check if epoch exists")
		return
	}

	if !exists {
		proto.ErrorMsg(c, "epoch does not exist in database")
		return
	}

	// Step 2: Retrieve data based on claimed status
	var records []*psql.AirdropData
	switch request.Claimed {
	case 0:
		records, err = database.GetAllAirdropDataByEpoch(request.Epoch)
	case 1:
		records, err = database.GetClaimedAirdropDataByEpoch(request.Epoch, true)
	case 2:
		records, err = database.GetClaimedAirdropDataByEpoch(request.Epoch, false)
	default:
		proto.ErrorMsg(c, "invalid claimed status")
		return
	}

	if err != nil {
		logrus.WithError(err).Error("Failed to retrieve airdrop data")
		proto.ErrorMsg(c, "failed to retrieve airdrop data")
		return
	}

	// Step 3: Write data to CSV file
	file, err := os.Create(request.CsvFile)
	if err != nil {
		logrus.WithError(err).Error("Failed to create CSV file")
		proto.ErrorMsg(c, "failed to create CSV file")
		return
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	if err := writer.Write([]string{"address", "amount"}); err != nil {
		logrus.WithError(err).Error("Failed to write CSV header")
		proto.ErrorMsg(c, "failed to write CSV header")
		return
	}

	for _, record := range records {
		if err := writer.Write([]string{record.Address, record.Amount}); err != nil {
			logrus.WithError(err).Error("Failed to write CSV record")
			proto.ErrorMsg(c, "failed to write CSV record")
			return
		}
	}

	proto.SuccessMsg(c, http.StatusOK, "Airdrop data exported successfully", nil)
}

func deleteAirdropHandler(c *gin.Context) {
	var request struct {
		Epoch uint64 `json:"epoch"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		logrus.WithError(err).Error("Failed to bind JSON")
		proto.ErrorMsg(c, "failed to bind JSON")
		return
	}

	// Step 1: Check if epoch exists in the database (optimized)
	exists, err := database.CheckEpochExists(request.Epoch)
	if err != nil {
		logrus.WithError(err).Error("Failed to check if epoch exists")
		proto.ErrorMsg(c, "failed to check if epoch exists")
		return
	}

	if !exists {
		proto.ErrorMsg(c, "epoch does not exist in database")
		return
	}

	// Step 2: Delete all records for the specified epoch
	if err := database.DeleteAirdropDataByEpoch(request.Epoch); err != nil {
		logrus.WithError(err).Error("Failed to delete airdrop data")
		proto.ErrorMsg(c, "failed to delete airdrop data")
		return
	}

	proto.SuccessMsg(c, http.StatusOK, "Airdrop data deleted successfully", nil)
}

func importAirdropHandler(c *gin.Context) {
	var request struct {
		Epoch   uint64 `json:"epoch"`
		CsvFile string `json:"csvFile"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		logrus.WithError(err).Error("Failed to bind JSON")
		proto.ErrorMsg(c, "failed to bind JSON")
		return
	}

	// Step 1: Check epoch validity
	proxy := contracts.GetProxy()
	currentEpoch, err := proxy.GetCurrentEpoch()
	if err != nil {
		logrus.WithError(err).Error("Failed to get current epoch")
		proto.ErrorMsg(c, "failed to get current epoch")
		return
	}

	if request.Epoch > currentEpoch+1 {
		proto.ErrorMsg(c, "invalid epoch, should be less than or equal to current epoch + 1")
		return
	}

	exists, err := database.CheckEpochValidity(request.Epoch)
	if err != nil {
		logrus.WithError(err).Error("Failed to check epoch validity in database")
		proto.ErrorMsg(c, "failed to check epoch validity in database")
		return
	}
	if !exists {
		proto.ErrorMsg(c, "epoch already exists in database")
		return
	}

	// Step 2: Read CSV file and batch insert into database
	file, err := os.Open(request.CsvFile)
	if err != nil {
		logrus.WithError(err).Error("Failed to open CSV file")
		proto.ErrorMsg(c, "failed to open CSV file")
		return
	}
	defer file.Close()

	reader := csv.NewReader(file)
	records, err := reader.ReadAll()
	if err != nil {
		logrus.WithError(err).Error("Failed to read CSV file")
		proto.ErrorMsg(c, "failed to read CSV file")
		return
	}

	if len(records) < 2 {
		proto.ErrorMsg(c, "CSV file is empty or contains only header")
		return
	}

	airdropRecords := make([]*psql.AirdropData, 0, len(records)-1)
	for i, record := range records[1:] {
		if len(record) != 2 {
			proto.ErrorMsg(c, fmt.Sprintf("invalid CSV record format at line %d", i+2))
			return
		}
		address := strings.ToLower(record[0])
		amount, ok := new(big.Int).SetString(record[1], 10)
		if !ok {
			proto.ErrorMsg(c, fmt.Sprintf("invalid amount format at line %d", i+2))
			return
		}

		airdropRecords = append(airdropRecords, &psql.AirdropData{
			Epoch:     request.Epoch,
			Address:   address,
			Amount:    amount.String(),
			Claimed:   false,
			CreatedAt: time.Now().Unix(),
		})
	}

	if err := database.BatchCreateAirdropData(airdropRecords); err != nil {
		logrus.WithError(err).Error("Failed to save airdrop data")
		proto.ErrorMsg(c, "failed to save airdrop data")
		return
	}

	proto.SuccessMsg(c, http.StatusOK, "Airdrop data imported successfully", nil)
}

func slackBotEventHandler(c *gin.Context) {
	fmt.Println("GGGGGG")
	config := &botConfig.Config{
		BotToken:      os.Getenv("SLACK_BOT_TOKEN"),
		SigningSecret: os.Getenv("SLACK_SIGNING_SECRET"),
	}

	fmt.Println(os.Getenv("SLACK_BOT_TOKEN"))
	fmt.Println(os.Getenv("SLACK_SIGNING_SECRET"))

	handler := bot.NewSlackEventHandler(config)

	handler.ServeHTTP(c.Writer, c.Request)
}
