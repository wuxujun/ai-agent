package promptmanager

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/policy"
)

var publicPromptClient = policy.ConfiguredHTTPClient(3*time.Second, false)
var privatePromptClient = policy.ConfiguredHTTPClient(3*time.Second, true)

func (pm *PromptManager) doPromptRequest(req *http.Request) (*http.Response, error) {
	base := pm.client
	if base == nil {
		base = publicPromptClient
		if config.Get().Langfuse.AllowPrivateNetwork {
			base = privatePromptClient
		}
	}
	if err := policy.ValidateConfiguredURL(req.URL.String(), true); err != nil {
		return nil, errors.New("invalid prompt endpoint")
	}
	client := *base
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client.Do(req)
}

func decodePromptResponse(body io.Reader, dest any) error {
	const maxPromptBytes = 1 << 20
	data, err := io.ReadAll(io.LimitReader(body, maxPromptBytes+1))
	if err != nil {
		return errors.New("read prompt response failed")
	}
	if len(data) > maxPromptBytes {
		return errors.New("prompt response exceeds size limit")
	}
	if err := json.Unmarshal(data, dest); err != nil {
		return errors.New("invalid prompt response JSON")
	}
	return nil
}
