package cmd

/*
	Command: import
	Description: Calculates the Merkle root from an attached CSV file and imports it.
	The CSV file should contain two columns: 'address' and 'amount'.
	Usage: @<bot_name> import --epoch <epoch_number> [--update] (with a CSV file attached)
	Example: @bot import --epoch 1

	Flags:
	  --epoch <number>: (Required) The epoch to process.
	  --update: (Optional) If provided, updates the existing Merkle tree for the current epoch instead of importing a new one.
*/

import (
	"bytes"
	"encoding/csv"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"strings"

	"github.com/Bedrock-Technology/VeMerkle/internal/contracts"
	merkletree "github.com/FantasyJony/openzeppelin-merkle-tree-go/standard_merkle_tree"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/slack-go/slack"
)

// NewImportCommand creates the import command.
func NewImportCommand() Command {
	return Command{
		Name:        "import",
		Description: "Calculates the Merkle root from an attached CSV file.",
		Usage: `Calculates the Merkle root from a CSV file.

*Flags:*
  --epoch <number>: (Required) The epoch to process.`,
		Execute: runImportCommand,
	}
}

func runImportCommand(ctx CommandContext) (string, error) {
	// 1. Create a new FlagSet.
	importFlagSet := flag.NewFlagSet("import", flag.ContinueOnError)

	// 2. Define the flags.
	epoch := importFlagSet.Uint64("epoch", math.MaxUint64, "The epoch to process (required).")

	// 3. Parse the arguments.
	if err := importFlagSet.Parse(ctx.Args); err != nil {
		return "", fmt.Errorf("error parsing flags. Use `help import` for usage details")
	}

	// 4. Validate the flags.
	if *epoch == math.MaxUint64 {
		return "", fmt.Errorf("the --epoch flag is required. Use `help import` for usage details")
	}

	if len(ctx.FullMessage.Files) == 0 {
		return "", fmt.Errorf("you must attach a CSV file to use this command")
	}

	var csvFile *slack.File
	for _, file := range ctx.FullMessage.Files {
		if strings.HasSuffix(strings.ToLower(file.Name), ".csv") {
			csvFile = &file
			break
		}
	}

	if csvFile == nil {
		return "", fmt.Errorf("no .csv file found in the attachments")
	}

	// Use a goroutine for the heavy lifting.
	args := processImportArgs{
		file:  *csvFile,
		epoch: *epoch,
	}
	go processImport(ctx, args)

	return "Request received. I'm processing the file now and will post the results shortly.", nil
}

// processImport handles the downloading, processing, and posting the result.
type processImportArgs struct {
	file  slack.File
	epoch uint64
}

func processImport(ctx CommandContext, args processImportArgs) {
	channelID := ctx.AppMentionEvent.Channel
	timestamp := ctx.AppMentionEvent.TimeStamp

	// 1. Download file content
	body, err := downloadFile(ctx, args.file.URLPrivateDownload)
	if err != nil {
		slog.Error("Error downloading file", "error", err, "file_url", args.file.URLPrivateDownload)
		postMessage(ctx.Client, channelID, "Sorry, I couldn't download the file.", timestamp, MessageTypeError)
		return
	}

	records, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
	if err != nil {
		slog.Error("Error reading CSV file", "error", err)
		postMessage(ctx.Client, channelID, fmt.Sprintf("I had trouble reading the CSV file: %v", err), timestamp, MessageTypeError)
		return
	}

	if len(records) == 0 {
		slog.Error("Error reading CSV file", "error", err)
		postMessage(ctx.Client, channelID, "The CSV file is empty.", timestamp, MessageTypeError)
		return
	}

	proxy := contracts.GetProxy()
	isActive, err := proxy.IsCurrentEpochActive()

	if err != nil {
		slog.Error("Error checking if current epoch is active", "error", err)
		postMessage(ctx.Client, channelID, fmt.Sprintf("I had trouble checking if the current epoch is active: %v", err), timestamp, MessageTypeError)
		return
	}

	if isActive {
		slog.Error("The current epoch is already active.")
		postMessage(ctx.Client, channelID, "The current epoch is active.", timestamp, MessageTypeError)
		return
	}

	// Skip CSV header
	records = records[1:]

	// 2. Build Merkle tree
	merkleTree, err := parseCSVAndBuildMerkleTree(records)
	if err != nil {
		slog.Error("Error creating Merkle tree", "error", err)
		postMessage(ctx.Client, channelID, fmt.Sprintf("I had trouble generating the Merkle root: %v", err), timestamp, MessageTypeError)
		return
	}

	// 3. Dispatch to the correct handler
	handlerErr := handleImport(ctx, merkleTree, args.epoch)

	if handlerErr != nil {
		slog.Error("Error processing merkletree", "error", handlerErr, "channel", channelID)
		postMessage(ctx.Client, channelID, fmt.Sprintf("Sorry, an error occurred: %v", handlerErr), timestamp, MessageTypeError)
		return
	}

	merkleRoot := merkleTree.Tree.GetRoot()
	hexMerkleRoot := hexutil.Encode(merkleRoot)

	postMessage(ctx.Client, channelID, "Merkle root successfully calculated: "+hexMerkleRoot, timestamp, MessageTypeInfo)
}

func handleImport(ctx CommandContext, merkleTree *MerkleTree, epoch uint64) error {
	if err := MerkleTreeManager.Import(merkleTree, epoch); err != nil {
		return err
	}

	slog.Info("Successfully calculated Merkle root for import", "channel", ctx.AppMentionEvent.Channel, "epoch", epoch)
	return nil
}

func parseCSVAndBuildMerkleTree(records [][]string) (*MerkleTree, error) {
	leaves := [][]any{}
	addressMap := make(map[string]int)
	amountMap := make(map[string]*big.Int)

	for i, record := range records {
		address := strings.ToLower(record[0])
		amountStr := record[1]

		amount, ok := new(big.Int).SetString(amountStr, 10)
		if !ok {
			return nil, fmt.Errorf("invalid amount in record %d: %s", i, amountStr)
		}

		leaves = append(leaves, []any{
			merkletree.SolAddress(address),
			merkletree.SolNumber(amountStr),
		})

		addressMap[address] = i
		amountMap[address] = amount
	}

	tree, err := merkletree.Of(leaves, []string{
		merkletree.SOL_ADDRESS, merkletree.SOL_UINT256,
	})
	if err != nil {
		return nil, err
	}

	return &MerkleTree{
		Tree:    tree,
		Address: addressMap,
		Amount:  amountMap,
	}, nil
}
