package gateway

import (
	"net/http"

	"github.com/yzx/rl-router/internal/domain"
	"github.com/yzx/rl-router/pkg/jsonutil"
)

const resourceGroupHeader = "X-InferRouter-Resource-Group"

// extractSessionID extracts the optional session_id from an OpenAI-compatible request body.
// Returns empty string if not present or on parse error.
func extractSessionID(bodyBytes []byte) string {
	var meta struct {
		SessionID string `json:"session_id"`
	}
	// Ignore errors - clients may not send session_id (backward compatible).
	_ = jsonutil.Unmarshal(bodyBytes, &meta)
	return meta.SessionID
}

// extractModel extracts the optional model from an OpenAI-compatible request body.
// Returns empty string if not present or on parse error.
func extractModel(bodyBytes []byte) string {
	var meta struct {
		Model string `json:"model"`
	}
	_ = jsonutil.Unmarshal(bodyBytes, &meta)
	return meta.Model
}

func extractResourceGroup(r *http.Request, bodyBytes []byte) string {
	if r != nil {
		if group := r.Header.Get(resourceGroupHeader); group != "" {
			return group
		}
		if r.URL != nil {
			if group := r.URL.Query().Get("resource_group"); group != "" {
				return group
			}
		}
	}
	var meta struct {
		ResourceGroup string `json:"resource_group"`
		Metadata      struct {
			ResourceGroup string `json:"resource_group"`
		} `json:"metadata"`
	}
	_ = jsonutil.Unmarshal(bodyBytes, &meta)
	if meta.ResourceGroup != "" {
		return meta.ResourceGroup
	}
	if meta.Metadata.ResourceGroup != "" {
		return meta.Metadata.ResourceGroup
	}
	return domain.DefaultResourceGroup
}
