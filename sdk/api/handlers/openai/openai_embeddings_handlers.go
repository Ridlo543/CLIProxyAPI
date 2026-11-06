package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
)

var cfRoundRobinCounter uint64

// OpenAIEmbeddingItem represents a single embedding vector in an OpenAI response
type OpenAIEmbeddingItem struct {
	Object    string    `json:"object"`
	Index     int       `json:"index"`
	Embedding []float64 `json:"embedding"`
}

// OpenAIEmbeddingResponse represents the standard OpenAI /v1/embeddings response payload
type OpenAIEmbeddingResponse struct {
	Object string                `json:"object"`
	Data   []OpenAIEmbeddingItem `json:"data"`
	Model  string                `json:"model"`
	Usage  map[string]int        `json:"usage"`
}

// Embeddings handles the POST /v1/embeddings endpoint.
// It translates incoming OpenAI embedding requests (string or array input)
// to Cloudflare Workers AI or other compatible providers and returns standard OpenAI embeddings format.
func (h *OpenAIAPIHandler) Embeddings(c *gin.Context) {
	rawJSON, err := handlers.ReadRequestBody(c)
	if err != nil {
		c.JSON(clienterror.HTTPStatusFromErrorOr(err, http.StatusBadRequest), handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("Invalid request: %v", err),
				Type:    "invalid_request_error",
			},
		})
		return
	}

	model := strings.TrimSpace(ojson.GetBytesNoCopy(rawJSON, "model").String())
	if model == "" {
		model = "@cf/baai/bge-m3"
	}

	// Extract input: can be a single string or an array of strings
	var inputTexts []string
	inputField := ojson.GetBytesNoCopy(rawJSON, "input")
	if inputField.Type == ojson.String {
		s := inputField.String()
		if s != "" {
			inputTexts = append(inputTexts, s)
		}
	} else if inputField.IsArray() {
		for _, item := range inputField.Array() {
			if item.Type == ojson.String {
				inputTexts = append(inputTexts, item.String())
			}
		}
	}

	if len(inputTexts) == 0 {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "input is required and must not be empty",
				Type:    "invalid_request_error",
			},
		})
		return
	}

	// Look up available Cloudflare Workers AI credentials from AuthManager
	type cfAccount struct {
		accountID string
		apiToken  string
	}
	var cfAccounts []cfAccount

	if h.AuthManager != nil {
		for _, a := range h.AuthManager.List() {
			if a == nil || a.Disabled {
				continue
			}
			baseURL := ""
			apiToken := ""
			if a.Attributes != nil {
				baseURL = strings.TrimSpace(a.Attributes["base_url"])
				apiToken = strings.TrimSpace(a.Attributes["api_key"])
			}
			if strings.Contains(strings.ToLower(baseURL), "api.cloudflare.com") && strings.Contains(baseURL, "/accounts/") {
				parts := strings.Split(baseURL, "/accounts/")
				if len(parts) == 2 {
					accID := strings.Split(parts[1], "/")[0]
					if accID != "" && apiToken != "" {
						cfAccounts = append(cfAccounts, cfAccount{accountID: accID, apiToken: apiToken})
					}
				}
			}
		}
	}

	if len(cfAccounts) == 0 {
		c.JSON(http.StatusServiceUnavailable, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "No active Cloudflare Workers AI accounts configured for embeddings",
				Type:    "server_error",
			},
		})
		return
	}

	// Clean model name: strip provider prefix if present (e.g. "cloudflare-ainything/@cf/baai/bge-m3" -> "@cf/baai/bge-m3")
	cleanModel := model
	if idx := strings.Index(cleanModel, "@cf/"); idx > 0 {
		cleanModel = cleanModel[idx:]
	}

	// Round-robin selection across available Cloudflare accounts
	idx := atomic.AddUint64(&cfRoundRobinCounter, 1) - 1
	selectedAccount := cfAccounts[idx%uint64(len(cfAccounts))]

	// Cloudflare Workers AI embedding endpoint
	cfURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/ai/run/%s", selectedAccount.accountID, cleanModel)
	cfReqBody, _ := json.Marshal(map[string]any{
		"text": inputTexts,
	})

	httpReq, errReq := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, cfURL, bytes.NewReader(cfReqBody))
	if errReq != nil {
		c.JSON(http.StatusInternalServerError, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("Failed to construct upstream request: %v", errReq),
				Type:    "server_error",
			},
		})
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+selectedAccount.apiToken)
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, errDo := client.Do(httpReq)
	if errDo != nil {
		c.JSON(http.StatusBadGateway, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("Upstream Cloudflare request failed: %v", errDo),
				Type:    "upstream_error",
			},
		})
		return
	}
	defer resp.Body.Close()

	respBytes, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		c.JSON(http.StatusBadGateway, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Failed to read Cloudflare response body",
				Type:    "upstream_error",
			},
		})
		return
	}

	if resp.StatusCode != http.StatusOK {
		c.Data(resp.StatusCode, "application/json", respBytes)
		return
	}

	// Parse Cloudflare Workers AI result: {"result": {"shape": [N, Dims], "data": [[...], [...]]}, "success": true}
	var cfResult struct {
		Success bool `json:"success"`
		Result  struct {
			Data  [][]float64 `json:"data"`
			Shape []int       `json:"shape"`
		} `json:"result"`
		Errors []any `json:"errors"`
	}

	if errUnmarshal := json.Unmarshal(respBytes, &cfResult); errUnmarshal != nil || !cfResult.Success || len(cfResult.Result.Data) == 0 {
		c.JSON(http.StatusBadGateway, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Invalid embedding result from Cloudflare Workers AI",
				Type:    "upstream_error",
			},
		})
		return
	}

	// Assemble OpenAI embedding response
	openAIResp := OpenAIEmbeddingResponse{
		Object: "list",
		Data:   make([]OpenAIEmbeddingItem, len(cfResult.Result.Data)),
		Model:  cleanModel,
		Usage: map[string]int{
			"prompt_tokens": len(inputTexts) * 8, // estimate
			"total_tokens":  len(inputTexts) * 8,
		},
	}

	for i, vec := range cfResult.Result.Data {
		openAIResp.Data[i] = OpenAIEmbeddingItem{
			Object:    "embedding",
			Index:     i,
			Embedding: vec,
		}
	}

	c.JSON(http.StatusOK, openAIResp)
}
