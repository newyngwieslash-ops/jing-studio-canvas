// 从 GitHub 安装与同步技能：解析仓库地址、下载归档、按提交号记录版本。

package skills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"yingce/backend/internal/kernel"
	"yingce/backend/internal/outbound"
)

func parseGitHubSkillURL(rawURL string, requestedRef string, requestedSubdir string) (githubSkillSpec, error) {
	target, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || target.Scheme != "https" || !strings.EqualFold(target.Hostname(), "github.com") || target.User != nil {
		return githubSkillSpec{}, kernel.BadAuthRequest("请输入公开的 https://github.com 仓库地址")
	}
	parts := strings.Split(strings.Trim(target.Path, "/"), "/")
	if len(parts) < 2 {
		return githubSkillSpec{}, kernel.BadAuthRequest("GitHub 地址必须包含 owner/repository")
	}
	spec := githubSkillSpec{Owner: parts[0], Repo: strings.TrimSuffix(parts[1], ".git"), Ref: strings.TrimSpace(requestedRef), Subdir: strings.Trim(strings.ReplaceAll(strings.TrimSpace(requestedSubdir), "\\", "/"), "/")}
	if len(parts) >= 4 && parts[2] == "tree" {
		if spec.Ref == "" {
			spec.Ref = parts[3]
		}
		if spec.Subdir == "" && len(parts) > 4 {
			spec.Subdir = strings.Join(parts[4:], "/")
		}
	}
	if len(parts) > 2 && parts[2] != "tree" {
		return githubSkillSpec{}, kernel.BadAuthRequest("GitHub 地址必须指向仓库根目录或 tree 子目录")
	}
	if spec.Owner == "" || spec.Repo == "" {
		return githubSkillSpec{}, kernel.BadAuthRequest("GitHub 仓库地址无效")
	}
	return spec, nil
}

func fetchGitHubSkillArchive(ctx context.Context, spec githubSkillSpec) (skillPackageArchive, string, string, string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resolvedRef := strings.TrimSpace(spec.Ref)
	downloadRef := resolvedRef
	if downloadRef == "" {
		downloadRef = "HEAD"
	}
	data, err := downloadGitHubZip(ctx, client, spec.Owner, spec.Repo, downloadRef)
	if err != nil {
		return skillPackageArchive{}, "", "", "", err
	}
	commitSHA := extractGitHubZipCommit(data)
	if commitSHA == "" {
		if resolvedRef == "" {
			var repoInfo struct {
				DefaultBranch string `json:"default_branch"`
			}
			if err := githubJSON(ctx, client, fmt.Sprintf("https://api.github.com/repos/%s/%s", url.PathEscape(spec.Owner), url.PathEscape(spec.Repo)), &repoInfo); err != nil {
				return skillPackageArchive{}, "", "", "", err
			}
			resolvedRef = repoInfo.DefaultBranch
		}
		var commitInfo struct {
			SHA string `json:"sha"`
		}
		if err := githubJSON(ctx, client, fmt.Sprintf("https://api.github.com/repos/%s/%s/commits/%s", url.PathEscape(spec.Owner), url.PathEscape(spec.Repo), url.PathEscape(resolvedRef)), &commitInfo); err != nil {
			return skillPackageArchive{}, "", "", "", err
		}
		commitSHA = strings.TrimSpace(commitInfo.SHA)
	}
	if commitSHA == "" {
		return skillPackageArchive{}, "", "", "", errors.New("GitHub 未返回提交版本")
	}
	archive, err := archiveFromZip(data, spec.Subdir)
	if err != nil {
		return skillPackageArchive{}, "", "", "", err
	}
	canonicalURL := fmt.Sprintf("https://github.com/%s/%s", spec.Owner, spec.Repo)
	return archive, commitSHA, canonicalURL, resolvedRef, nil
}

func downloadGitHubZip(ctx context.Context, client *http.Client, owner string, repo string, ref string) ([]byte, error) {
	downloadURL := fmt.Sprintf("https://codeload.github.com/%s/%s/zip/%s", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(ref))
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	request.Header.Set("User-Agent", outbound.DefaultOutboundUserAgent)
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("下载 GitHub 技能失败: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, kernel.BadAuthRequest("GitHub 仓库、分支或标签不存在，请检查仓库地址并确认仓库已公开")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("下载 GitHub 技能失败: HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxSkillPackageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSkillPackageBytes {
		return nil, kernel.BadAuthRequest("GitHub 技能包超过 20MB")
	}
	return data, nil
}

func githubJSON(ctx context.Context, client *http.Client, target string, output any) error {
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", outbound.DefaultOutboundUserAgent)
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("读取 GitHub 信息失败: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("读取 GitHub 信息失败: HTTP %d", response.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(output)
}
