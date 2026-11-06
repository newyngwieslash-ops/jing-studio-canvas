package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"yingce/backend/internal/assets"
	"yingce/backend/internal/model"
)

// ResourceForUser 是 Provider worker 读取资源时必须经过的 service 层归属校验。
func (s *Service) ResourceForUser(actor *model.User, id string) (*model.Resource, error) {
	if actor == nil || actor.ID == "" {
		return nil, errors.New("用户身份无效")
	}
	return s.repo.ResourceForUser(actor.ID, id)
}

// providerResourceURL 只能接收已经完成用户归属和 ready 状态校验的资源，
// 并为上游签发短时地址；它不是绕过权限校验的通用资源 URL 生成器。
// expiresAt 为零值时使用后台「厂商访问地址有效期」（默认 4 小时），非零时只能进一步缩短。
func (s *Service) providerResourceURL(resource *model.Resource, expiresAt time.Time) (string, error) {
	access, err := s.resolveResourceAccess(resource, ResourceAccessOptions{Purpose: assets.PurposeProvider, ExpiresAt: expiresAt})
	if err != nil && resource != nil && resource.Provider == "local" {
		if attempted, promoteErr := s.promoteLocalResourceToActiveOSS(resource); promoteErr != nil {
			return "", promoteErr
		} else if attempted {
			access, err = s.resolveResourceAccess(resource, ResourceAccessOptions{Purpose: assets.PurposeProvider, ExpiresAt: expiresAt})
		}
	}
	if err != nil {
		return "", err
	}
	return access.URL, nil
}

// promoteLocalResourceToActiveOSS 在本地资源缺少公网访问根地址、但当前已启用对象存储时，
// 按需将该历史本地资源上传并切换到当前生效的对象存储位置。
func (s *Service) promoteLocalResourceToActiveOSS(resource *model.Resource) (bool, error) {
	if resource == nil || resource.Provider != "local" || resource.Status != model.ResourceStatusReady || strings.TrimSpace(resource.ObjectKey) == "" {
		return false, nil
	}
	setting, storageSettingID, useOSS, err := s.activeResourceOSSSetting(resource.UserID)
	if err != nil {
		return true, err
	}
	if !useOSS {
		return false, nil
	}
	localObjectKey := resource.ObjectKey
	file, err := os.Open(filepath.Join(s.dataDir, "resources", filepath.FromSlash(localObjectKey)))
	if err != nil {
		return true, fmt.Errorf("读取本地参考资源失败：%w", err)
	}
	now := time.Now().UTC()
	targetObjectKey := ossObjectKey(setting, resource.UserID, resource.Kind, filepath.Base(localObjectKey), resource.MimeType, now)
	key := storageBreakerKey(&model.Resource{Provider: setting.Provider, Endpoint: setting.Endpoint, Bucket: setting.Bucket})
	uploadCtx, cancel := context.WithTimeout(context.Background(), resourceOffloadUploadTimeout)
	etag, err := putOSSObjectContext(uploadCtx, s.storageSettingWithRuntimePolicy(setting), targetObjectKey, resource.MimeType, resource.Size, file)
	cancel()
	_ = file.Close()
	mediaStorageBreaker.record(key, err)
	if err != nil {
		return true, fmt.Errorf("同步本地参考资源到对象存储失败：%w", err)
	}
	deleteAfter := now.Add(resourceOffloadLocalRetention)
	job := &model.ResourceOffloadJob{
		ID:                     newID(),
		UserID:                 resource.UserID,
		ResourceID:             resource.ID,
		Size:                   resource.Size,
		LocalObjectKey:         localObjectKey,
		LocalPlaybackKey:       resource.PlaybackObjectKey,
		TargetProvider:         setting.Provider,
		TargetEndpoint:         setting.Endpoint,
		TargetBucket:           setting.Bucket,
		TargetStorageSettingID: storageSettingID,
		TargetObjectKey:        targetObjectKey,
		Status:                 model.ResourceOffloadStatusDone,
		NextAttemptAt:          now,
		LocalDeleteAfter:       &deleteAfter,
		CreatedAt:              now,
		UpdatedAt:              now,
	}
	switched, err := s.repo.SwitchResourceToObjectStorage(job, etag)
	if err != nil {
		_ = deleteOSSObject(s.storageSettingWithRuntimePolicy(setting), targetObjectKey)
		return true, fmt.Errorf("切换参考资源存储位置失败：%w", err)
	}
	if !switched {
		_ = deleteOSSObject(s.storageSettingWithRuntimePolicy(setting), targetObjectKey)
		latest, readErr := s.repo.ResourceForUser(resource.UserID, resource.ID)
		if readErr != nil {
			return true, readErr
		}
		*resource = *latest
		return true, nil
	}
	if err := s.repo.UpsertResourceOffloadJob(job); err != nil {
		slog.Warn("resource offload record after promotion failed", "resourceId", resource.ID, "error", err)
	}
	resource.Provider = setting.Provider
	resource.Endpoint = setting.Endpoint
	resource.Bucket = setting.Bucket
	resource.StorageSettingID = storageSettingID
	resource.ObjectKey = targetObjectKey
	resource.ETag = etag
	resource.PlaybackStatus = model.PlaybackStatusNone
	resource.PlaybackObjectKey = ""
	resource.PlaybackError = ""
	return true, nil
}
