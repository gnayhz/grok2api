package media

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

var (
	ErrInputImageURLInvalid        = errors.New("图片 URL 无效，仅支持无凭据的 http/https 80/443 地址")
	ErrInputImageURLBlocked        = errors.New("目标地址不允许访问")
	ErrInputImageTooLarge          = errors.New("图片超过大小上限")
	ErrInputImageSourceUnavailable = errors.New("图片导入不可用")
	ErrInputImageFetch             = errors.New("下载图片失败")
)

const MaxInputImageURLBytes = 8192

// InputImageSource fetches bounded bytes through the image import policy. It
// owns network resources and never registers an asset or applies storage policy.
type InputImageSource interface {
	FetchImage(context.Context, *url.URL, int64) ([]byte, error)
}

func ParseInputImageURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) > MaxInputImageURLBytes {
		return nil, ErrInputImageURLInvalid
	}
	parsed, err := url.Parse(raw)
	if err != nil || !AllowedInputImageURL(parsed) {
		return nil, ErrInputImageURLInvalid
	}
	return parsed, nil
}

// AllowedInputImageURL applies to the initial URL and every redirect. Address
// resolution and the actual connected IP must also pass the public network guard.
func AllowedInputImageURL(parsed *url.URL) bool {
	if parsed == nil || (!strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https")) || parsed.Hostname() == "" || parsed.User != nil {
		return false
	}
	if port := parsed.Port(); port != "" && port != "80" && port != "443" {
		return false
	}
	return true
}
