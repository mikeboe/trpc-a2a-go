package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/mikeboe/trpc-a2a-go/log"
	"github.com/mikeboe/trpc-a2a-go/protocol"
	"github.com/mikeboe/trpc-a2a-go/server"
	"github.com/mikeboe/trpc-a2a-go/taskmanager"
	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/googleai"
)

// exchangeProcessor implements the taskmanager.TaskProcessor interface.
type exchangeProcessor struct {
	llm llms.Model
}

// newExchangeProcessor creates a new exchange processor with LangChain.
func newExchangeProcessor() (*exchangeProcessor, error) {
	// Initialize Google Gemini model
	llm, err := googleai.New(
		context.Background(),
		googleai.WithAPIKey(getAPIKey()),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize Gemini model: %w", err)
	}

	return &exchangeProcessor{
		llm: llm,
	}, nil
}

func getAPIKey() string {
	return os.Getenv("GOOGLE_API_KEY")
}

// Process implements the taskmanager.TaskProcessor interface.
func (p *exchangeProcessor) Process(
	ctx context.Context,
	taskID string,
	message protocol.Message,
	handle taskmanager.TaskHandle,
) error {
	// Extract text from the incoming message.
	query := extractText(message)
	if query == "" {
		errMsg := "input message must contain text."
		log.Error("Task %s failed: %s", taskID, errMsg)

		// Update status to Failed via handle.
		failedMessage := protocol.NewMessage(
			protocol.MessageRoleAgent,
			[]protocol.Part{protocol.NewTextPart(errMsg)},
		)
		_ = handle.UpdateStatus(protocol.TaskStateFailed, &failedMessage)
		return fmt.Errorf(errMsg)
	}

	log.Info("Processing exchange task %s with query: %s", taskID, query)

	// First attempt to use the LLM to enhance understanding
	prompt := fmt.Sprintf(
		"Your task is to determine what currencies the user wants to convert "+
			"between and for what date. Extract ONLY the following information in JSON format:\n"+
			"- from_currency: The 3-letter currency code to convert from\n"+
			"- to_currency: The 3-letter currency code to convert to\n"+
			"- date: The date if specified (in YYYY-MM-DD format), or 'latest' if not specified\n\n"+
			"User query: %s\n\n"+
			"Respond only with valid JSON. If you cannot determine a field, use default values "+
			"(USD for from_currency, EUR for to_currency, latest for date).",
		query,
	)

	// Try to get a structured response from the LLM
	var fromCurrency, toCurrency, date string
	completion, err := llms.GenerateFromSinglePrompt(ctx, p.llm, prompt)
	if err == nil {
		// Try to parse the JSON response
		var response struct {
			FromCurrency string `json:"from_currency"`
			ToCurrency   string `json:"to_currency"`
			Date         string `json:"date"`
		}

		if err := json.Unmarshal([]byte(completion), &response); err == nil {
			fromCurrency = response.FromCurrency
			toCurrency = response.ToCurrency
			date = response.Date
		}
	}

	// If LLM parsing failed, fall back to simple parsing
	if fromCurrency == "" || toCurrency == "" {
		fromCurrency, toCurrency, date = parseExchangeQuery(query)
	}

	// Handle non-currency questions with the LLM
	if !strings.Contains(strings.ToLower(query), "currency") &&
		!strings.Contains(strings.ToLower(query), "exchange") &&
		!strings.Contains(strings.ToLower(query), "convert") &&
		!strings.Contains(strings.ToLower(query), "rate") {

		log.Info("Non-currency question detected, using LLM to respond")

		prompt = fmt.Sprintf(
			"The user has asked: '%s'\n\n"+
				"If this is asking about currency exchange rates, respond normally. "+
				"If this is asking about ANYTHING else, politely explain that you are a specialized "+
				"currency exchange assistant and can only answer questions about currency exchange rates. "+
				"Do not attempt to answer unrelated questions.\n\n"+
				"Your response:",
			query,
		)

		completion, err := llms.GenerateFromSinglePrompt(ctx, p.llm, prompt)
		if err == nil && !strings.Contains(strings.ToLower(completion), "exchange rate") {
			// The LLM indicated this wasn't about exchange rates
			responseMessage := protocol.NewMessage(
				protocol.MessageRoleAgent,
				[]protocol.Part{protocol.NewTextPart(completion)},
			)
			return handle.UpdateStatus(protocol.TaskStateCompleted, &responseMessage)
		}
	}

	// Get the exchange rate
	result, err := getExchangeRate(fromCurrency, toCurrency, date)
	if err != nil {
		log.Error("Exchange rate error: %v", err)
		errorMessage := protocol.NewMessage(
			protocol.MessageRoleAgent,
			[]protocol.Part{protocol.NewTextPart(fmt.Sprintf("Error processing request: %v", err))},
		)
		return handle.UpdateStatus(protocol.TaskStateFailed, &errorMessage)
	}

	// Format response with some explanation
	finalResponse := fmt.Sprintf("Based on your query, I looked up the exchange rate from %s to %s.\n\n%s",
		fromCurrency, toCurrency, result)

	log.Info("Responding with: %s", finalResponse)

	// Update task status to completed with the response
	responseMessage := protocol.NewMessage(
		protocol.MessageRoleAgent,
		[]protocol.Part{protocol.NewTextPart(finalResponse)},
	)

	if err := handle.UpdateStatus(protocol.TaskStateCompleted, &responseMessage); err != nil {
		return fmt.Errorf("failed to update task status: %w", err)
	}

	// Add the exchange rate data as an artifact
	artifact := protocol.Artifact{
		Name:        stringPtr("Exchange Rate Data"),
		Description: stringPtr(fmt.Sprintf("Exchange rate from %s to %s", fromCurrency, toCurrency)),
		Index:       0,
		Parts:       []protocol.Part{protocol.NewTextPart(result)},
		LastChunk:   boolPtr(true),
	}

	if err := handle.AddArtifact(artifact); err != nil {
		log.Error("Error adding artifact for task %s: %v", taskID, err)
	}

	return nil
}

// parseExchangeQuery attempts to parse a natural language query to extract currency info.
func parseExchangeQuery(query string) (fromCurrency, toCurrency, date string) {
	// Default currencies if we can't parse anything better
	fromCurrency = "USD"
	toCurrency = "EUR"

	// Lowercase and simplify the query
	query = strings.ToLower(query)
	words := strings.Fields(query)

	// Look for currency codes (3-letter codes like USD, EUR, GBP, etc.)
	currencyCodes := []string{"usd", "eur", "gbp", "jpy", "aud", "cad",
		"chf", "cny", "hkd", "nzd", "sek", "krw", "sgd", "nok", "mxn", "inr", "rub", "zar", "try", "brl", "twd"}

	foundCurrencies := []string{}
	for _, word := range words {
		// Clean up the word from punctuation
		word = strings.Trim(word, ",.?!:;()[]{}\"'")

		for _, code := range currencyCodes {
			if strings.Contains(word, code) {
				foundCurrencies = append(foundCurrencies, strings.ToUpper(code))
				break
			}
		}

		// Look for date pattern (YYYY-MM-DD)
		if len(word) == 10 && strings.Count(word, "-") == 2 {
			date = word
		}
	}

	// Set the found currencies
	if len(foundCurrencies) >= 2 {
		fromCurrency = foundCurrencies[0]
		toCurrency = foundCurrencies[1]
	} else if len(foundCurrencies) == 1 {
		// If only one currency found, assume it's the target
		toCurrency = foundCurrencies[0]
	}

	return
}

// getExchangeRate fetches exchange rate information from Frankfurter API.
func getExchangeRate(fromCurrency, toCurrency, date string) (string, error) {
	fromCurrency = strings.ToUpper(fromCurrency)
	toCurrency = strings.ToUpper(toCurrency)

	if date == "" {
		date = "latest"
	}

	url := fmt.Sprintf("https://api.frankfurter.app/%s?from=%s&to=%s",
		date, fromCurrency, toCurrency)

	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("failed to parse JSON: %w", err)
	}

	// Format response in a more readable way
	if rates, ok := result["rates"].(map[string]interface{}); ok {
		// For a more readable output format
		baseAmount := 1.0
		if amount, ok := result["amount"].(float64); ok {
			baseAmount = amount
		}

		var rateStr string
		for currency, rate := range rates {
			rateStr += fmt.Sprintf("%v %s = %v %s\n", baseAmount, fromCurrency, rate, currency)
		}

		date := result["date"]
		return fmt.Sprintf("Exchange rate on %v:\n%s", date, rateStr), nil
	}

	// Fallback to raw JSON if format is different
	jsonResult, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to format result: %w", err)
	}

	return string(jsonResult), nil
}

// extractText extracts the text content from a message.
func extractText(message protocol.Message) string {
	for _, part := range message.Parts {
		if textPart, ok := part.(protocol.TextPart); ok {
			return textPart.Text
		}
	}
	return ""
}

// Helper functions
func stringPtr(s string) *string {
	return &s
}

func boolPtr(b bool) *bool {
	return &b
}

// getAgentCard returns the agent's metadata.
func getAgentCard() server.AgentCard {
	return server.AgentCard{
		Name:        "Currency Exchange Agent",
		Description: stringPtr("An agent that can fetch and display currency exchange rates."),
		URL:         "http://localhost:8081",
		Version:     "1.0.0",
		Capabilities: server.AgentCapabilities{
			Streaming:              false,
			PushNotifications:      false,
			StateTransitionHistory: true,
		},
		DefaultInputModes:  []string{string(protocol.PartTypeText)},
		DefaultOutputModes: []string{string(protocol.PartTypeText)},
		Skills: []server.AgentSkill{
			{
				ID:          "exchange_rate",
				Name:        "Currency Exchange Rates",
				Description: stringPtr("Gets the current or historical exchange rates between currencies."),
				Examples: []string{
					"What is the exchange rate from USD to EUR?",
					"Convert 100 USD to JPY",
					"What was the rate of GBP to USD on 2023-01-01?",
				},
			},
		},
	}
}

func main() {
	// Parse command-line flags
	host := flag.String("host", "localhost", "Host to listen on")
	port := flag.Int("port", 8081, "Port to listen on for the exchange rate agent")
	flag.Parse()

	// Create the exchange processor
	processor, err := newExchangeProcessor()
	if err != nil {
		log.Fatal("Failed to create exchange processor: %v", err)
	}

	// Create task manager and inject processor
	taskManager, err := taskmanager.NewMemoryTaskManager(processor)
	if err != nil {
		log.Fatal("Failed to create task manager: %v", err)
	}

	// Create the A2A server
	agentCard := getAgentCard()
	a2aServer, err := server.NewA2AServer(agentCard, taskManager)
	if err != nil {
		log.Fatal("Failed to create A2A server: %v", err)
	}

	// Set up a channel to listen for termination signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start the server in a goroutine
	go func() {
		serverAddr := fmt.Sprintf("%s:%d", *host, *port)
		log.Info("Starting Currency Exchange Agent server on %s", serverAddr)
		if err := a2aServer.Start(serverAddr); err != nil {
			log.Fatal("Server failed: %v", err)
		}
	}()

	// Wait for termination signal
	sig := <-sigChan
	log.Info("Received signal %v, shutting down...", sig)
}
