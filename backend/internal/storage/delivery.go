package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mime"
	"net/url"
	"strings"
	"time"
)

const (
	aliyunOSSThumbnailProcess = "image/resize,w_640,m_lfit/format,webp"
	tencentCOSThumbnailQuery  = "imageMogr2/thumbnail/640x/format/webp"
	qiniuDoraThumbnailQuery   = "imageView2/2/w/640/format/webp"
)

// DeliverySettings is independent from origin credentials. Public CDN access must be an explicit choice.
type DeliverySettings struct {
	CDNAuthMode       string `json:"cdnAuthMode"`
	RequireCDN        bool   `json:"requireCDN"`
	AllowPrivateProxy bool   `json:"allowPrivateProxy"`
}

func CDNEnabled(setting Settings) bool {
	if setting.CDNBaseURL == "" {
		return false
	}
	return setting.Delivery.CDNAuthMode == "public" || setting.Delivery.CDNAuthMode == "qiniu"
}

// SupportsCloudThumbnail 判断当前对象存储与 MIME 类型是否支持云端光栅缩略图直签。
// 仅限 JPEG/PNG/WebP（排除 SVG、GIF 动图、易触发云端 400 或浏览器无法解码的 BMP/TIFF 及非图片格式）；
// 七牛未启用 CDN 时回落到 S3 兼容源站（不支持 Dora imageView2），故返回 false。
func SupportsCloudThumbnail(setting Settings, mimeType string) bool {
	if strings.TrimSpace(setting.Provider) == "" && strings.TrimSpace(setting.Endpoint) == "" && strings.TrimSpace(setting.CDNBaseURL) == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(mimeType))
	if err != nil {
		return false
	}
	switch mediaType {
	case "image/jpeg", "image/png", "image/webp":
	default:
		return false
	}
	setting = NormalizeSettings(setting)
	switch setting.Provider {
	case aliyunOSSProvider, tencentCOSProvider:
		return true
	case qiniuKodoProvider:
		return CDNEnabled(setting)
	default:
		return false
	}
}

func SignCDNURL(setting Settings, objectKey string, expires time.Time) (string, error) {
	switch setting.Delivery.CDNAuthMode {
	case "public":
		return OssCDNObjectURL(setting.CDNBaseURL, objectKey)
	case "qiniu":
		if setting.Provider == qiniuKodoProvider {
			return SignedQiniuObjectURL(setting, objectKey, expires)
		}
	}
	return "", errors.New("CDN 用户访问鉴权方式未配置或不支持")
}

func SignCDNThumbnailURL(setting Settings, objectKey string, expires time.Time) (string, error) {
	setting = NormalizeSettings(setting)
	switch setting.Delivery.CDNAuthMode {
	case "public":
		return publicCDNObjectThumbnailURL(setting, objectKey)
	case "qiniu":
		if setting.Provider == qiniuKodoProvider {
			return signedQiniuCDNThumbnailURL(setting, objectKey, expires)
		}
	}
	return "", errors.New("CDN 用户访问鉴权方式未配置或不支持")
}

func publicCDNObjectThumbnailURL(setting Settings, objectKey string) (string, error) {
	value, err := OssCDNObjectURL(setting.CDNBaseURL, objectKey)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", err
	}
	switch setting.Provider {
	case aliyunOSSProvider:
		query := parsed.Query()
		query.Set("x-oss-process", aliyunOSSThumbnailProcess)
		parsed.RawQuery = query.Encode()
	case tencentCOSProvider:
		parsed.RawQuery = tencentCOSThumbnailQuery
	case qiniuKodoProvider:
		parsed.RawQuery = qiniuDoraThumbnailQuery
	default:
		return "", errors.New("当前对象存储不支持云端缩略图参数")
	}
	return parsed.String(), nil
}

func PublicOrigin(setting Settings) bool {
	if setting.Provider == qiniuKodoProvider {
		return true
	} // The SDK signs the public Kodo S3 origin, not its upload endpoint.
	if setting.Provider == volcengineTOSProvider && volcengineTOSInternalEndpoint(setting.Endpoint) {
		return false // ivolces.com 只在火山引擎内网可达，浏览器拿到的预签名地址打不开。
	}
	return PublicHTTPSStorageEndpoint(setting.Endpoint)
}

func DeliveryRevision(setting Settings) string {
	// Only a digest leaves the server; changes to credentials/configuration invalidate descriptors.
	data, _ := json.Marshal(setting)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}
