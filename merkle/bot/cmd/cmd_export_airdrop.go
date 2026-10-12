package cmd

import (
	"bytes"
	"encoding/csv"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"strings"

	"github.com/Bedrock-Technology/VeMerkle/internal/config"
	"github.com/Bedrock-Technology/VeMerkle/internal/contracts"
	"github.com/Bedrock-Technology/VeMerkle/internal/database"
	"github.com/jszwec/csvutil"
	"github.com/slack-go/slack"
)

// NewExportAirdropCommand creates the export_airdrop command.
func NewExportAirdropCommand() Command {
	return Command{
		Name:        "export_airdrop",
		Description: "Exports airdrop data to a CSV file.",
		Usage: `Exports airdrop data to a CSV file.

*Flags:*
  --epoch <number>: (Required) The epoch to process.`,
		Execute: runExportAirdropCommand,
	}
}

func runExportAirdropCommand(ctx CommandContext) (string, error) {
	// triggers the processing in a separate goroutine and returns a confirmation message. Errors in
	// parsing or missing flags are returned with appropriate messages.

	flagSet := flag.NewFlagSet("export_airdrop", flag.ContinueOnError)

	// 2. Define the flags.
	epoch := flagSet.Uint64("epoch", math.MaxUint64, "The epoch to process (required).")

	// 3. Parse the arguments.
	if err := flagSet.Parse(ctx.Args); err != nil {
		return "", fmt.Errorf("error parsing flags. Use `help export_airdrop` for usage details")
	}

	// 4. Validate the flags.
	if *epoch == math.MaxUint64 {
		return "", fmt.Errorf("the --epoch flag is required. Use `help export_airdrop` for usage details")
	}

	if len(ctx.FullMessage.Files) == 0 {
		return "", fmt.Errorf("no CSV file provided. Use `help export_airdrop` for usage details")
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

	proxy := contracts.GetProxy()
	currentEpoch, err := proxy.GetCurrentEpoch()
	if err != nil {
		slog.Error("Failed to get current epoch", slog.Any("error", err))
		return "", fmt.Errorf("Failed to get current epoch")
	}

	// Use a goroutine for the heavy lifting.
	args := processExportAirdropArgs{
		epoch: *epoch,
		file:  *csvFile,
	}
	go func() {
		if args.epoch == currentEpoch+1 {
			if err := processUpdateClaim(ctx, args.epoch-1); err != nil {
				slog.Error("Failed to update claim", "error", err)
				return
			}
		} else if args.epoch > currentEpoch+1 {
			slog.Error("Invalid epoch", "epoch", args.epoch)
			postMessage(ctx.Client, ctx.AppMentionEvent.Channel, fmt.Sprintf("Epoch is not allowed: `%d`, current epoch: `%d`", args.epoch, currentEpoch), ctx.AppMentionEvent.TimeStamp, MessageTypeError)
			return
		}

		processExportAirdrop(ctx, args)
	}()

	return "Request received. I'm processing the airdrop export now and will post the results shortly.", nil
}

type processExportAirdropArgs struct {
	epoch uint64
	file  slack.File
}

func processExportAirdrop(ctx CommandContext, args processExportAirdropArgs) {
	channelID := ctx.AppMentionEvent.Channel
	timestamp := ctx.AppMentionEvent.TimeStamp

	// 0. Refresh available amount of unclaimed addresses, expiring stale balances
	expireEpochs := config.GetConfig().Airdrop.ExpireEpochs
	if err := database.RefreshAvailableAmount(args.epoch-1, expireEpochs); err != nil {
		slog.Error("Failed to refresh available amount", "error", err)
		postMessage(ctx.Client, channelID, fmt.Sprintf("Failed to refresh available amount: %v", err), timestamp, MessageTypeError)
		return
	}

	// 1. Get unclaimed airdrop data from database
	unclaimedAirdropData, err := database.GetAvailableAirdropDataByEpoch(args.epoch - 1)
	if err != nil {
		slog.Error("Failed to get airdrop data", "error", err)
		postMessage(ctx.Client, channelID, fmt.Sprintf("Failed to get airdrop data: %v", err), timestamp, MessageTypeError)
		return
	}

	if len(unclaimedAirdropData) == 0 {
		postMessage(ctx.Client, channelID, "No airdrop data found for the specified epoch and claim type.", timestamp, MessageTypeError)
		return
	}

	// 2. Get uploaded CSV file
	csvContent, err := downloadFile(ctx, args.file.URLPrivateDownload)
	if err != nil {
		slog.Error("Failed to download CSV file", "error", err)
		postMessage(ctx.Client, channelID, "Failed to download CSV file.", timestamp, MessageTypeError)
		return
	}

	type CurrentAirdrop struct {
		Address string `csv:"address"`
		Amount  string `csv:"amount"`
	}

	var currentAirdrops []CurrentAirdrop
	if err := csvutil.Unmarshal(csvContent, &currentAirdrops); err != nil {
		slog.Error("Failed to unmarshal CSV file", "error", err)
		postMessage(ctx.Client, channelID, "Failed to unmarshal CSV file.", timestamp, MessageTypeError)
		return
	}

	// 3. Merge airdrop data
	mergedAirdropData := make(map[string]string)
	for _, data := range unclaimedAirdropData {
		mergedAirdropData[strings.ToLower(data.Address)] = data.Amount
	}

	for _, data := range currentAirdrops {
		if amount, ok := mergedAirdropData[strings.ToLower(data.Address)]; ok {
			newAmount, err := sumAmounts(amount, data.Amount)
			if err != nil {
				slog.Error("Failed to sum amounts",
					slog.String("address", data.Address),
					slog.String("current-amount", data.Amount),
					slog.String("unclaimed-amount", amount),
					slog.Any("error", err))
				continue
			}
			mergedAirdropData[strings.ToLower(data.Address)] = newAmount
		} else {
			mergedAirdropData[strings.ToLower(data.Address)] = data.Amount
		}
	}

	// 4. Create CSV file and calculate stats
	var buffer bytes.Buffer
	writer := csv.NewWriter(&buffer)

	// Write header
	header := []string{"Address", "Amount"}
	if err := writer.Write(header); err != nil {
		slog.Error("Failed to write CSV header", "error", err)
		postMessage(ctx.Client, channelID, "Failed to generate CSV file.", timestamp, MessageTypeError)
		return
	}

	totalAmount := new(big.Int)
	// Write records
	for address, amountStr := range mergedAirdropData {
		record := []string{address, amountStr}
		if err := writer.Write(record); err != nil {
			slog.Error("Failed to write CSV record", "error", err)
			postMessage(ctx.Client, channelID, "Failed to generate CSV file.", timestamp, MessageTypeError)
			return
		}
		amount, ok := new(big.Int).SetString(amountStr, 10)
		if !ok {
			slog.Error("Failed to parse amount string during summary calculation", "amount", amountStr, "address", address)
			continue
		}
		totalAmount.Add(totalAmount, amount)
	}
	writer.Flush()

	// 5. Upload file to Slack
	fileName := fmt.Sprintf("airdrop_epoch_%d.csv", args.epoch)
	if err := uploadFileToSlack(
		ctx,
		channelID,
		timestamp,
		fileName,
		"Epoch Airdrop",
		"Airdrop data for epoch",
		&buffer,
	); err != nil {
		postMessage(ctx.Client, channelID, "Failed to upload CSV file to Slack.", timestamp, MessageTypeError)
		return
	}

	slog.Info("Airdrop data exported successfully", "epoch", args.epoch)

	// 6. Post summary message
	divisor := new(big.Float).SetPrec(256).SetFloat64(1e18)
	totalAmountFloat := new(big.Float).SetPrec(256).SetInt(totalAmount)
	totalAmountEther := new(big.Float).Quo(totalAmountFloat, divisor)

	// Format the amount for better readability
	formattedAmount := totalAmountEther.Text('f', 18)
	formattedAmount = strings.TrimRight(formattedAmount, "0")
	formattedAmount = strings.TrimRight(formattedAmount, ".")

	summaryMessage := fmt.Sprintf("Export Summary: Total Addresses: %d, Total Amount: %s.",
		len(mergedAirdropData),
		formattedAmount,
	)
	postMessage(ctx.Client, channelID, summaryMessage, timestamp, MessageTypeInfo)
}

func sumAmounts(a, b string) (string, error) {
	valA, ok := new(big.Int).SetString(a, 10)
	if !ok {
		return "", fmt.Errorf("invalid amount format: %s", a)
	}
	valB, ok := new(big.Int).SetString(b, 10)
	if !ok {
		return "", fmt.Errorf("invalid amount format: %s", b)
	}
	sum := new(big.Int).Add(valA, valB)
	return sum.String(), nil
}
