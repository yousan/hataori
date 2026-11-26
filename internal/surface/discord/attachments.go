package discord

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OpenAttachment uses no bot credential: Discord's signed CDN addresses need
// none. Its transport is independent of the authenticated REST client.
func (d *Driver) OpenAttachment(ctx context.Context, address string) (io.ReadCloser, error) {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Fragment != "" ||
		(u.Host != "cdn.discordapp.com" && u.Host != "media.discordapp.net") || !strings.HasPrefix(u.Path, "/attachments/") {
		return nil, fmt.Errorf("attachment address is not on a service attachment host")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, fmt.Errorf("attachment response: HTTP %d", res.StatusCode)
	}
	return res.Body, nil
}
