package storage

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"yingce/backend/internal/outbound/outboundtest"
)

// 展示地址的签名时间由调用方对齐；同一时间窗内重复签发必须得到逐字节相同的 URL，
// 否则浏览器 HTTP 缓存（以完整 URL 为键）在每次刷新画布时都会失效并重新下载对象。
func TestDisplayURLIsDeterministicForAlignedSigningTime(t *testing.T) {
	t.Setenv("CANVAS_ALLOWED_PRIVATE_UPSTREAM_HOSTS", "127.0.0.1")
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	signedAt := time.Now().UTC().Truncate(time.Hour)
	expiresAt := signedAt.Add(24 * time.Hour)
	cases := map[string]Settings{
		"s3":      {Provider: s3Provider, Region: "cn-nb1", Endpoint: server.URL, Bucket: "bucket", AccessKeyID: "access-id", AccessKeySecret: "secret-value"},
		"qiniu":   {Provider: qiniuKodoProvider, Endpoint: "https://up-z0.qiniup.com", Bucket: "bucket", AccessKeyID: "access-id", AccessKeySecret: "secret-value"},
		"tencent": {Provider: tencentCOSProvider, Endpoint: "https://cos.ap-guangzhou.myqcloud.com", Bucket: "bucket-1250000000", AccessKeyID: "access-id", AccessKeySecret: "secret-value"},
		"aliyun":  {Provider: aliyunOSSProvider, Endpoint: "https://oss-cn-test.aliyuncs.com", Bucket: "bucket", AccessKeyID: "access-id", AccessKeySecret: "secret-value"},
	}
	for name, setting := range cases {
		t.Run(name, func(t *testing.T) {
			first, err := SignedOriginObjectDisplayURL(setting, "users/u-1/image/a b.png", signedAt, expiresAt)
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(1100 * time.Millisecond) // 跨过秒级时间戳，证明结果不依赖 time.Now。
			second, err := SignedOriginObjectDisplayURL(setting, "users/u-1/image/a b.png", signedAt, expiresAt)
			if err != nil {
				t.Fatal(err)
			}
			if first != second {
				t.Fatalf("display URL is not stable:\n%s\n%s", first, second)
			}
			query := mustParseQuery(t, first)
			if got := query.Get("response-cache-control"); got != "private, max-age=86400, immutable" {
				t.Fatalf("response-cache-control = %q in %s", got, first)
			}
			if strings.Contains(first, "secret-value") {
				t.Fatal("signed URL leaked secret key")
			}
			switch name {
			case "s3", "qiniu":
				if query.Get("X-Amz-Date") != signedAt.Format("20060102T150405Z") || query.Get("X-Amz-Expires") != "86400" {
					t.Fatalf("SigV4 window = %s/%s", query.Get("X-Amz-Date"), query.Get("X-Amz-Expires"))
				}
			case "tencent":
				window := strings.Join([]string{itoa(signedAt.Unix()), itoa(expiresAt.Unix())}, ";")
				if query.Get("q-sign-time") != window || query.Get("q-key-time") != window {
					t.Fatalf("COS window = %s", first)
				}
			}
		})
	}
}

func TestOneShotURLsKeepCurrentTimeSigningWithoutCacheOverride(t *testing.T) {
	setting := Settings{Provider: qiniuKodoProvider, Endpoint: "https://up-z0.qiniup.com", Bucket: "bucket", AccessKeyID: "access-id", AccessKeySecret: "secret-value"}
	value, err := SignedOriginObjectDownloadURL(setting, "users/u-1/video/clip.mp4", time.Now().Add(time.Hour), "镜头.mp4")
	if err != nil {
		t.Fatal(err)
	}
	query := mustParseQuery(t, value)
	if query.Get("response-cache-control") != "" || !strings.HasPrefix(query.Get("response-content-disposition"), "attachment") {
		t.Fatalf("download URL = %s", value)
	}
	signedAt, err := time.Parse("20060102T150405Z", query.Get("X-Amz-Date"))
	if err != nil || time.Since(signedAt) > time.Minute {
		t.Fatalf("download URL must be signed at the current time, got %q", query.Get("X-Amz-Date"))
	}
}

func TestPublicCDNObjectDownloadURLForcesAttachmentOnlyOnDownloadURL(t *testing.T) {
	setting := Settings{CDNBaseURL: "https://media.example.com"}
	value, err := PublicCDNObjectDownloadURL(setting, "users/u-1/image/test image.png", "下载图片.png")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != "media.example.com" || parsed.Path != "/users/u-1/image/test image.png" || !strings.HasPrefix(parsed.Query().Get("response-content-disposition"), "attachment") {
		t.Fatalf("download URL = %q", value)
	}
	inline, err := OssCDNObjectURL(setting.CDNBaseURL, "users/u-1/image/test image.png")
	if err != nil {
		t.Fatal(err)
	}
	if parsedInline, err := url.Parse(inline); err != nil || parsedInline.RawQuery != "" {
		t.Fatalf("inline CDN URL = %q, %v", inline, err)
	}
}

func mustParseQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Query()
}

func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}

func TestSupportsCloudThumbnailPolicy(t *testing.T) {
	outboundtest.PublicDNS(t, "oss-cn-hangzhou.aliyuncs.com", "cos.ap-guangzhou.myqcloud.com", "s3.amazonaws.com", "tos-s3-cn-beijing.volces.com")
	aliyun := Settings{Provider: aliyunOSSProvider, Endpoint: "https://oss-cn-hangzhou.aliyuncs.com", Bucket: "b", AccessKeyID: "ak", AccessKeySecret: "sk"}
	tencent := Settings{Provider: tencentCOSProvider, Endpoint: "https://cos.ap-guangzhou.myqcloud.com", Bucket: "b-1250000000", AccessKeyID: "ak", AccessKeySecret: "sk"}
	qiniuPrivateCDN := Settings{Provider: qiniuKodoProvider, Endpoint: "https://up-z0.qiniup.com", CDNBaseURL: "https://cdn.example.com", Delivery: DeliverySettings{CDNAuthMode: "qiniu"}, Bucket: "b", AccessKeyID: "ak", AccessKeySecret: "sk"}
	qiniuPublicCDN := Settings{Provider: qiniuKodoProvider, Endpoint: "https://up-z0.qiniup.com", CDNBaseURL: "https://cdn.example.com", Delivery: DeliverySettings{CDNAuthMode: "public"}, Bucket: "b"}

	for _, mimeType := range []string{"image/jpeg", "image/png", "image/webp", "IMAGE/PNG; charset=binary"} {
		for name, setting := range map[string]Settings{
			"aliyun":           aliyun,
			"tencent":          tencent,
			"qiniu_private":    qiniuPrivateCDN,
			"qiniu_public_cdn": qiniuPublicCDN,
		} {
			if !SupportsCloudThumbnail(setting, mimeType) {
				t.Fatalf("SupportsCloudThumbnail(%s, %q) = false, want true", name, mimeType)
			}
		}
	}

	for _, mimeType := range []string{"image/bmp", "image/tiff", "image/svg+xml", "image/svg", "image/gif", "video/mp4", "audio/mpeg", "application/pdf", "application/octet-stream", ""} {
		if SupportsCloudThumbnail(aliyun, mimeType) {
			t.Fatalf("SupportsCloudThumbnail(aliyun, %q) = true, want false", mimeType)
		}
	}

	// Qiniu without CDN falls back to *.s3.*.qiniucs.com which does NOT support Dora imageView2.
	qiniuNoCDN := Settings{Provider: qiniuKodoProvider, Endpoint: "https://up-z0.qiniup.com", Bucket: "b", AccessKeyID: "ak", AccessKeySecret: "sk"}
	qiniuIncompleteCDN := Settings{Provider: qiniuKodoProvider, Endpoint: "https://up-z0.qiniup.com", CDNBaseURL: "https://cdn.example.com", Bucket: "b", AccessKeyID: "ak", AccessKeySecret: "sk"}
	if SupportsCloudThumbnail(qiniuNoCDN, "image/png") || SupportsCloudThumbnail(qiniuIncompleteCDN, "image/png") {
		t.Fatal("qiniu without enabled CDN must not claim cloud thumbnail support")
	}

	for name, setting := range map[string]Settings{
		"empty":      {},
		"local":      {Provider: "local"},
		"s3":         {Provider: s3Provider, Region: "us-east-1", Endpoint: "https://s3.amazonaws.com", Bucket: "b", AccessKeyID: "ak", AccessKeySecret: "sk"},
		"volcengine": {Provider: volcengineTOSProvider, Region: "cn-beijing", Endpoint: "https://tos-s3-cn-beijing.volces.com", Bucket: "b", AccessKeyID: "ak", AccessKeySecret: "sk"},
	} {
		if SupportsCloudThumbnail(setting, "image/png") {
			t.Fatalf("SupportsCloudThumbnail(%s) = true, want false", name)
		}
	}
}

func TestSignedAliyunOSSThumbnailSignatureVector(t *testing.T) {
	setting := Settings{
		Provider:        aliyunOSSProvider,
		Endpoint:        "https://oss-cn-hangzhou.aliyuncs.com",
		Bucket:          "canvas-prod",
		AccessKeyID:     "ak-test",
		AccessKeySecret: "sk-test",
	}
	objectKey := "users/u-1/image/a b.png"
	signedAt := time.Unix(1_999_987_200, 0).UTC()
	expiresAt := signedAt.Add(24 * time.Hour) // 2000073600

	got, err := SignedOriginObjectThumbnailDisplayURL(setting, objectKey, signedAt, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	query := mustParseQuery(t, got)
	wantCache := "private, max-age=86400, immutable"
	wantProcess := "image/resize,w_640,m_lfit/format,webp"
	if query.Get("response-cache-control") != wantCache {
		t.Fatalf("response-cache-control = %q, want %q", query.Get("response-cache-control"), wantCache)
	}
	if query.Get("x-oss-process") != wantProcess {
		t.Fatalf("x-oss-process = %q, want %q", query.Get("x-oss-process"), wantProcess)
	}

	canonical := "/canvas-prod/users/u-1/image/a b.png?response-cache-control=" + wantCache + "&x-oss-process=" + wantProcess
	stringToSign := strings.Join([]string{http.MethodGet, "", "", "2000073600", canonical}, "\n")
	mac := hmac.New(sha1.New, []byte("sk-test"))
	_, _ = mac.Write([]byte(stringToSign))
	wantSig := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if query.Get("Signature") != wantSig {
		t.Fatalf("Aliyun thumbnail display Signature = %q, want %q (stringToSign=%q)", query.Get("Signature"), wantSig, stringToSign)
	}

	// One-shot thumbnail URL (no response-cache-control) also signs unescaped x-oss-process sub-resource.
	oneShot, err := SignedOriginObjectThumbnailURL(setting, objectKey, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	oneShotQuery := mustParseQuery(t, oneShot)
	if oneShotQuery.Get("response-cache-control") != "" {
		t.Fatalf("one-shot thumbnail URL must not include response-cache-control: %s", oneShot)
	}
	oneShotCanonical := "/canvas-prod/users/u-1/image/a b.png?x-oss-process=" + wantProcess
	oneShotSTS := strings.Join([]string{http.MethodGet, "", "", "2000073600", oneShotCanonical}, "\n")
	mac2 := hmac.New(sha1.New, []byte("sk-test"))
	_, _ = mac2.Write([]byte(oneShotSTS))
	wantOneShotSig := base64.StdEncoding.EncodeToString(mac2.Sum(nil))
	if oneShotQuery.Get("Signature") != wantOneShotSig {
		t.Fatalf("Aliyun one-shot thumbnail Signature = %q, want %q", oneShotQuery.Get("Signature"), wantOneShotSig)
	}
}

func TestSignedCOSThumbnailURLIncludesCIParamAndAlignedWindow(t *testing.T) {
	setting := Settings{
		Provider:        tencentCOSProvider,
		Endpoint:        "https://cos.ap-guangzhou.myqcloud.com",
		Bucket:          "bucket-1250000000",
		AccessKeyID:     "cos-ak",
		AccessKeySecret: "cos-sk",
	}
	signedAt := time.Now().UTC().Truncate(time.Hour)
	expiresAt := signedAt.Add(24 * time.Hour)

	first, err := SignedOriginObjectThumbnailDisplayURL(setting, "users/u-1/image/a b.png", signedAt, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	query := mustParseQuery(t, first)
	const ciKey = "imageMogr2/thumbnail/640x/format/webp"
	if !query.Has(ciKey) {
		t.Fatalf("COS thumbnail URL missing %q: %s", ciKey, first)
	}
	if got := query.Get("response-cache-control"); got != "private, max-age=86400, immutable" {
		t.Fatalf("COS response-cache-control = %q", got)
	}
	window := strings.Join([]string{itoa(signedAt.Unix()), itoa(expiresAt.Unix())}, ";")
	if query.Get("q-sign-time") != window || query.Get("q-key-time") != window {
		t.Fatalf("COS window = %s/%s, want %s", query.Get("q-sign-time"), query.Get("q-key-time"), window)
	}
	paramList := query.Get("q-url-param-list")
	if !strings.Contains(paramList, "imagemogr2%2fthumbnail%2f640x%2fformat%2fwebp") || !strings.Contains(paramList, "response-cache-control") {
		t.Fatalf("COS q-url-param-list = %q, want CI param and response-cache-control", paramList)
	}
}

func TestSignCDNThumbnailURLQiniuPrivateSignatureVector(t *testing.T) {
	setting := Settings{
		Provider:        qiniuKodoProvider,
		CDNBaseURL:      "https://cdn.example.com",
		Delivery:        DeliverySettings{CDNAuthMode: "qiniu"},
		Bucket:          "bucket",
		AccessKeyID:     "qiniu-ak",
		AccessKeySecret: "qiniu-sk",
	}
	expiresAt := time.Unix(2_000_073_600, 0).UTC()
	got, err := SignCDNThumbnailURL(setting, "users/u-1/image/a b.png", expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "%3F") || strings.Contains(got, "%3f") {
		t.Fatalf("Qiniu thumbnail URL escaped '?' into object path: %s", got)
	}
	const wantPrefix = "https://cdn.example.com/users/u-1/image/a%20b.png?imageView2/2/w/640/format/webp&e=2000073600&token="
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("Qiniu thumbnail URL = %q, want prefix %q", got, wantPrefix)
	}
	beforeToken := strings.TrimSuffix(wantPrefix, "&token=")
	mac := hmac.New(sha1.New, []byte("qiniu-sk"))
	_, _ = mac.Write([]byte(beforeToken))
	wantToken := "qiniu-ak:" + base64.URLEncoding.EncodeToString(mac.Sum(nil))
	gotToken := strings.TrimPrefix(got, wantPrefix)
	if gotToken != wantToken {
		t.Fatalf("Qiniu token = %q, want %q", gotToken, wantToken)
	}
}

func TestThumbnailDisplayURLIsDeterministicForAlignedSigningTime(t *testing.T) {
	signedAt := time.Now().UTC().Truncate(time.Hour)
	expiresAt := signedAt.Add(24 * time.Hour)
	objectKey := "users/u-1/image/a b.png"

	originCases := map[string]Settings{
		"aliyun":  {Provider: aliyunOSSProvider, Endpoint: "https://oss-cn-test.aliyuncs.com", Bucket: "bucket", AccessKeyID: "access-id", AccessKeySecret: "secret-value"},
		"tencent": {Provider: tencentCOSProvider, Endpoint: "https://cos.ap-guangzhou.myqcloud.com", Bucket: "bucket-1250000000", AccessKeyID: "access-id", AccessKeySecret: "secret-value"},
	}
	cdnCases := map[string]struct {
		setting  Settings
		wantPath string
		wantSub  string
	}{
		"qiniu_private": {
			setting:  Settings{Provider: qiniuKodoProvider, CDNBaseURL: "https://cdn.example.com", Delivery: DeliverySettings{CDNAuthMode: "qiniu"}, Bucket: "bucket", AccessKeyID: "access-id", AccessKeySecret: "secret-value"},
			wantPath: "/users/u-1/image/a b.png",
			wantSub:  "imageView2/2/w/640/format/webp&e=" + itoa(expiresAt.Unix()) + "&token=",
		},
		"aliyun_public_cdn": {
			setting:  Settings{Provider: aliyunOSSProvider, CDNBaseURL: "https://cdn.example.com", Delivery: DeliverySettings{CDNAuthMode: "public"}},
			wantPath: "/users/u-1/image/a b.png",
			wantSub:  "x-oss-process=",
		},
		"tencent_public_cdn": {
			setting:  Settings{Provider: tencentCOSProvider, CDNBaseURL: "https://cdn.example.com", Delivery: DeliverySettings{CDNAuthMode: "public"}},
			wantPath: "/users/u-1/image/a b.png",
			wantSub:  "?imageMogr2/thumbnail/640x/format/webp",
		},
		"qiniu_public_cdn": {
			setting:  Settings{Provider: qiniuKodoProvider, CDNBaseURL: "https://cdn.example.com", Delivery: DeliverySettings{CDNAuthMode: "public"}},
			wantPath: "/users/u-1/image/a b.png",
			wantSub:  "?imageView2/2/w/640/format/webp",
		},
	}

	firstOrigin := make(map[string]string, len(originCases))
	for name, setting := range originCases {
		u, err := SignedOriginObjectThumbnailDisplayURL(setting, objectKey, signedAt, expiresAt)
		if err != nil {
			t.Fatalf("%s first error: %v", name, err)
		}
		firstOrigin[name] = u
	}
	firstCDN := make(map[string]string, len(cdnCases))
	for name, tc := range cdnCases {
		u, err := SignCDNThumbnailURL(tc.setting, objectKey, expiresAt)
		if err != nil {
			t.Fatalf("%s first error: %v", name, err)
		}
		firstCDN[name] = u
	}

	time.Sleep(1100 * time.Millisecond)

	for name, setting := range originCases {
		second, err := SignedOriginObjectThumbnailDisplayURL(setting, objectKey, signedAt, expiresAt)
		if err != nil {
			t.Fatalf("%s second error: %v", name, err)
		}
		if firstOrigin[name] != second {
			t.Fatalf("%s thumbnail origin URL not deterministic:\n%s\n%s", name, firstOrigin[name], second)
		}
	}
	for name, tc := range cdnCases {
		second, err := SignCDNThumbnailURL(tc.setting, objectKey, expiresAt)
		if err != nil {
			t.Fatalf("%s second error: %v", name, err)
		}
		if firstCDN[name] != second {
			t.Fatalf("%s thumbnail CDN URL not deterministic:\n%s\n%s", name, firstCDN[name], second)
		}
		parsed, err := url.Parse(second)
		if err != nil {
			t.Fatalf("%s invalid URL %q: %v", name, second, err)
		}
		if parsed.Path != tc.wantPath || !strings.Contains(second, tc.wantSub) {
			t.Fatalf("%s URL = %q (path=%q), want path=%q containing %q", name, second, parsed.Path, tc.wantPath, tc.wantSub)
		}
	}
}
