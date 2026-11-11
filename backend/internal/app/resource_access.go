package app

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"yingce/backend/internal/assets"
	"yingce/backend/internal/model"
	"yingce/backend/internal/storage"
)

type ResourceAccessOptions = assets.AccessOptions
type ResourceAccessRequest = assets.AccessRequest
type ResourceAccess = assets.ResourceAccess

type ResourceAccessFailure struct {
	Code    int    `json:"code"`
	Reason  string `json:"reason"`
	Message string `json:"msg"`
}
type ResourceAccessResult struct {
	ResourceID string                 `json:"resourceId"`
	Access     *ResourceAccess        `json:"access,omitempty"`
	Error      *ResourceAccessFailure `json:"error,omitempty"`
}

type resourceStorageMemoKey struct {
	userID           string
	provider         string
	storageSettingID string
	endpoint         string
	bucket           string
}

type cachedOSSSettingResult struct {
	setting ossSettingValue
	err     error
}

type resourceAccessBatchResolver struct {
	svc              *Service
	now              time.Time
	runtimeLoaded    bool
	runtimePolicy    storage.RuntimePolicy
	ossSettings      map[resourceStorageMemoKey]cachedOSSSettingResult
	publicBaseLoaded bool
	publicBaseURL    *url.URL
	publicBaseErr    error
}

func (b *resourceAccessBatchResolver) runtime() storage.RuntimePolicy {
	if !b.runtimeLoaded {
		b.runtimePolicy = b.svc.storageSettingWithRuntimePolicy(ossSettingValue{}).Runtime
		b.runtimeLoaded = true
	}
	return b.runtimePolicy
}

func (b *resourceAccessBatchResolver) publicBase() (*url.URL, error) {
	if !b.publicBaseLoaded {
		b.publicBaseURL, b.publicBaseErr = b.svc.publicResourceBaseURL()
		b.publicBaseLoaded = true
	}
	return b.publicBaseURL, b.publicBaseErr
}

func (b *resourceAccessBatchResolver) resolve(resource *model.Resource, options ResourceAccessOptions) (*ResourceAccess, error) {
	if resource == nil {
		return nil, NotFound("资源不存在")
	}
	var setting ossSettingValue
	runtimePolicy := b.runtime()
	if resource.Provider != "local" {
		memoKey := resourceStorageMemoKey{
			userID:           resource.UserID,
			provider:         strings.ToLower(strings.TrimSpace(resource.Provider)),
			storageSettingID: resource.StorageSettingID,
			endpoint:         strings.TrimRight(strings.TrimSpace(resource.Endpoint), "/"),
			bucket:           strings.TrimSpace(resource.Bucket),
		}
		cached, ok := b.ossSettings[memoKey]
		if !ok {
			rawSetting, err := b.svc.ossSettingForResource(resource.UserID, resource)
			if err == nil {
				rawSetting = normalizeOSSSetting(rawSetting)
				rawSetting.Runtime = runtimePolicy
				rawSetting = normalizeOSSSetting(rawSetting)
			}
			cached = cachedOSSSettingResult{setting: rawSetting, err: err}
			if b.ossSettings == nil {
				b.ossSettings = make(map[resourceStorageMemoKey]cachedOSSSettingResult, 4)
			}
			b.ossSettings[memoKey] = cached
		}
		if cached.err != nil {
			return nil, WrapAppError(503, "无法解析资源实际存储位置", cached.err)
		}
		setting = cached.setting
	} else {
		setting.Runtime = runtimePolicy
	}
	return assets.ResolveAccess(resource, setting, options, b.now, func(variant assets.ResourceVariant, expires time.Time) (string, error) {
		if options.Purpose == assets.PurposeEcommerceExport {
			u := &url.URL{Path: "/api/resources/" + url.PathEscape(resource.ID) + "/file"}
			q := u.Query()
			q.Set("purpose", string(options.Purpose))
			u.RawQuery = q.Encode()
			return u.String(), nil
		}
		if options.Purpose != assets.PurposeProvider {
			return b.svc.signedResourceAccessURL(resource, variant, expires, false)
		}
		rawPath, err := b.svc.signedResourceAccessURL(resource, variant, expires, false)
		if err != nil {
			return "", err
		}
		base, err := b.publicBase()
		if err != nil {
			return "", err
		}
		rel, err := url.Parse(rawPath)
		if err != nil {
			return "", err
		}
		return base.ResolveReference(rel).String(), nil
	})
}

func (s *Service) ResourceAccessBatch(userID string, requests []ResourceAccessRequest) ([]ResourceAccessResult, error) {
	if userID == "" {
		return nil, Unauthorized("请先登录")
	}
	if len(requests) == 0 || len(requests) > 100 {
		return nil, BadAuthRequest("每批资源访问请求须为 1–100 项")
	}

	normalized := make([]ResourceAccessOptions, len(requests))
	normErrs := make([]error, len(requests))
	uniqueIDs := make([]string, 0, len(requests))
	seenIDs := make(map[string]struct{}, len(requests))
	for i, request := range requests {
		options, err := assets.NormalizeAccessOptions(request.AccessOptions, false)
		normalized[i] = options
		normErrs[i] = err
		if err == nil && request.ResourceID != "" {
			if _, seen := seenIDs[request.ResourceID]; !seen {
				seenIDs[request.ResourceID] = struct{}{}
				uniqueIDs = append(uniqueIDs, request.ResourceID)
			}
		}
	}

	var resourceByID map[string]*model.Resource
	var batchErr error
	if len(uniqueIDs) > 0 {
		resources, err := s.repo.ResourcesForUserByIDs(userID, uniqueIDs)
		if err != nil {
			batchErr = err
		} else {
			resourceByID = make(map[string]*model.Resource, len(resources))
			for i := range resources {
				resourceByID[resources[i].ID] = &resources[i]
			}
		}
	}

	resolver := resourceAccessBatchResolver{
		svc: s,
		now: time.Now().UTC(),
	}
	results := make([]ResourceAccessResult, 0, len(requests))
	for i, request := range requests {
		item := ResourceAccessResult{ResourceID: request.ResourceID}
		err := normErrs[i]
		if err == nil {
			if batchErr != nil {
				err = batchErr
			} else if resource := resourceByID[request.ResourceID]; resource == nil {
				err = NotFound("资源不存在或不可访问")
			} else {
				item.Access, err = resolver.resolve(resource, normalized[i])
			}
		}
		if err != nil {
			item.Error = &ResourceAccessFailure{Code: 500, Reason: "resource_access_failed", Message: "资源访问失败"}
			var appErr *AppError
			if errors.As(err, &appErr) {
				item.Error = &ResourceAccessFailure{Code: appErr.Code, Reason: string(appErr.Reason), Message: appErr.Message}
			}
		}
		results = append(results, item)
	}
	return results, nil
}

func (s *Service) resolveResourceAccess(resource *model.Resource, options ResourceAccessOptions) (*ResourceAccess, error) {
	if resource == nil {
		return nil, NotFound("资源不存在")
	}
	var setting ossSettingValue
	if resource.Provider != "local" {
		var err error
		setting, err = s.ossSettingForResource(resource.UserID, resource)
		if err != nil {
			return nil, WrapAppError(503, "无法解析资源实际存储位置", err)
		}
		setting = s.storageSettingWithRuntimePolicy(setting)
	} else {
		// 本地资源无需存储配置，只注入运行时策略，让平台签名地址同样遵循后台配置的有效期。
		setting.Runtime = s.storageSettingWithRuntimePolicy(ossSettingValue{}).Runtime
	}
	return assets.ResolveAccess(resource, setting, options, time.Now().UTC(), func(variant assets.ResourceVariant, expires time.Time) (string, error) {
		if options.Purpose == assets.PurposeEcommerceExport {
			u := &url.URL{Path: "/api/resources/" + url.PathEscape(resource.ID) + "/file"}
			q := u.Query()
			q.Set("purpose", string(options.Purpose))
			u.RawQuery = q.Encode()
			return u.String(), nil
		}
		return s.signedResourceAccessURL(resource, variant, expires, options.Purpose == assets.PurposeProvider)
	})
}

func (s *Service) PrepareResourceDelivery(userID, id string, options ResourceAccessOptions, rangeHeader string) (*ResourceDelivery, error) {
	options, err := assets.NormalizeAccessOptions(options, false)
	if err != nil {
		return nil, err
	}
	resource, err := s.repo.ResourceForUser(userID, id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, NotFound("资源不存在或不可访问")
	}
	if err != nil {
		return nil, err
	}
	return s.prepareResourceDelivery(userID, resource, options, rangeHeader)
}

func (s *Service) prepareResourceDelivery(userID string, resource *model.Resource, options ResourceAccessOptions, rangeHeader string) (*ResourceDelivery, error) {
	if resource == nil || resource.UserID != userID {
		return nil, Forbidden("资源不可访问")
	}
	access, err := s.resolveResourceAccess(resource, options)
	if err != nil {
		return nil, err
	}
	delivery := &ResourceDelivery{Resource: resource, Access: access}
	if options.DescribeOnly || access.Delivery == assets.DeliveryCDN || access.Delivery == assets.DeliveryOrigin {
		return delivery, nil
	}
	if access.ActualVariant == assets.VariantPlayback {
		delivery.Stream, err = s.OpenResourcePlaybackRange(userID, resource.ID)
	} else {
		delivery.Stream, err = s.openResourceRange(userID, resource, rangeHeader)
	}
	if err != nil {
		return nil, err
	}
	delivery.Resource = delivery.Stream.Resource
	return delivery, nil
}

func (s *Service) signedResourceAccessURL(resource *model.Resource, variant assets.ResourceVariant, expires time.Time, public bool) (string, error) {
	expiry := strconv.FormatInt(expires.Unix(), 10)
	signature, err := s.signPublicResource(resource.ID+"\n"+string(variant), expiry)
	if err != nil {
		return "", err
	}
	u := &url.URL{Path: "/api/public/resources/" + url.PathEscape(resource.ID) + "/file"}
	q := u.Query()
	q.Set("expires", expiry)
	q.Set("signature", signature)
	q.Set("variant", string(variant))
	u.RawQuery = q.Encode()
	if !public {
		return u.String(), nil
	}
	base, err := s.publicResourceBaseURL()
	if err != nil {
		return "", err
	}
	return base.ResolveReference(u).String(), nil
}

func (s *Service) PreparePublicResourceDelivery(id, expires, signature string, options ResourceAccessOptions, rangeHeader string) (*ResourceDelivery, error) {
	options, err := assets.NormalizeAccessOptions(options, false)
	if err != nil {
		return nil, err
	}
	if err := s.verifyPublicResourceSignature(id+"\n"+string(options.Variant), expires, signature); err != nil {
		return nil, err
	}
	resource, err := s.repo.Resource(id)
	if err != nil {
		return nil, Forbidden("匿名下载链接无效")
	}
	seconds, _ := strconv.ParseInt(expires, 10, 64)
	options.ExpiresAt = time.Unix(seconds, 0)
	return s.prepareResourceDelivery(resource.UserID, resource, options, rangeHeader)
}
