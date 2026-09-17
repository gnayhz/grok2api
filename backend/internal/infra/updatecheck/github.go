// Package updatecheck implements the external release source.
package updatecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/pkg/texts"
	updatecheck "github.com/chenyme/grok2api/backend/internal/port/updatecheck"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	latestReleaseAPI = "https://api.github.com/repos/chenyme/grok2api/releases/latest"
	maxReleaseBytes  = 1 << 20
	maxNotesRunes    = 4096
)

type GitHubSource struct{ client *http.Client }

func NewGitHubSource(client *http.Client) *GitHubSource {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &GitHubSource{client: client}
}

func (s *GitHubSource) LatestRelease(ctx context.Context) (updatecheck.Release, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, latestReleaseAPI, nil)
	if err != nil {
		return updatecheck.Release{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	// UA 不携带精确版本号:该请求发往第三方 api.github.com, 版本号属于被动
	// 指纹; 检查结果只在管理端展示, UA 无需精确到版本。
	request.Header.Set("User-Agent", "grok2api/update-check")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := s.client.Do(request)
	if err != nil {
		return updatecheck.Release{}, fmt.Errorf("检查 GitHub Release 失败: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return updatecheck.Release{}, fmt.Errorf("GitHub Release 检查失败（HTTP %d）", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxReleaseBytes+1))
	if err != nil {
		return updatecheck.Release{}, fmt.Errorf("读取 GitHub Release 响应: %w", err)
	}
	if len(data) > maxReleaseBytes {
		return updatecheck.Release{}, errors.New("GitHub Release 响应超过安全上限")
	}
	var payload struct {
		Tag  string `json:"tag_name"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return updatecheck.Release{}, fmt.Errorf("解析 GitHub Release 响应: %w", err)
	}
	payload.Tag = strings.TrimSpace(payload.Tag)
	if payload.Tag == "" {
		return updatecheck.Release{}, errors.New("GitHub Release 未返回版本号")
	}
	return updatecheck.Release{
		Tag:   payload.Tag,
		URL:   "https://github.com/chenyme/grok2api/releases/tag/" + url.PathEscape(payload.Tag),
		Notes: texts.TruncateRunes(strings.TrimSpace(payload.Body), maxNotesRunes),
	}, nil
}
