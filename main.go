package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config 应用配置
type Config struct {
	Cookie        string `json:"cookie"`
	DownloadVideo bool   `json:"downloadVideo"`
	VideoEncoder  string `json:"videoEncoder"` // 如: libx264, h264_nvenc
	Concurrency   int    `json:"concurrency"`  // 并发数限制
	Timeout       int    `json:"timeout"`      // 单任务超时(秒)
}

// VideoInfo 视频分P信息
type VideoInfo struct {
	CID      string
	PartName string
	Index    int // 分P序号
}

// StreamURLs 音视频流地址
type StreamURLs struct {
	AudioURL string
	VideoURL string
}

// BilibiliClient B站API客户端
type BilibiliClient struct {
	httpClient *http.Client
	headers    map[string]string
	config     *Config
}

// NewBilibiliClient 创建客户端
func NewBilibiliClient(config *Config) *BilibiliClient {
	return &BilibiliClient{
		httpClient: &http.Client{
			Timeout: time.Duration(config.Timeout) * time.Second,
		},
		headers: getHeaders(config),
		config:  config,
	}
}

// getHeaders 生成请求头
func getHeaders(config *Config) map[string]string {
	return map[string]string{
		"accept":             "application/json, text/plain, */*",
		"accept-language":    "zh-CN,zh;q=0.9",
		"cookie":             config.Cookie,
		"origin":             "https://www.bilibili.com",
		"referer":            "https://www.bilibili.com",
		"sec-ch-ua":          `"Chromium";v="128"`,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"Windows"`,
		"sec-fetch-dest":     "empty",
		"sec-fetch-mode":     "cors",
		"sec-fetch-site":     "same-site",
		"user-agent":         "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36",
	}
}

// sanitizeFileName 清理文件名
func sanitizeFileName(name string) string {
	reg := regexp.MustCompile(`[<>:"/\\|?*]+`)
	return strings.TrimSpace(reg.ReplaceAllString(name, "_"))
}

// GetVideoInfo 获取视频分P列表
func (c *BilibiliClient) GetVideoInfo(bvid string) ([]VideoInfo, error) {
	url := fmt.Sprintf("https://api.bilibili.com/x/player/pagelist?bvid=%s", bvid)
	req, err := http.NewRequestWithContext(context.Background(), "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	for k, v := range c.headers {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API返回错误状态码: %d", resp.StatusCode)
	}

	var result struct {
		Code int `json:"code"`
		Data []struct {
			Cid  int    `json:"cid"`
			Part string `json:"part"`
		} `json:"data"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("解析JSON失败: %w", err)
	}

	if result.Code != 0 {
		return nil, fmt.Errorf("API返回错误: %s", result.Message)
	}

	var infos []VideoInfo
	for i, v := range result.Data {
		infos = append(infos, VideoInfo{
			CID:      strconv.Itoa(v.Cid),
			PartName: sanitizeFileName(v.Part),
			Index:    i + 1,
		})
	}

	return infos, nil
}

// GetStreamURLs 获取音视频流地址（带WBI签名）
func (c *BilibiliClient) GetStreamURLs(bvid, cid string) (*StreamURLs, error) {
	// 注意：B站新版playurl接口需要WBI签名
	// 这里使用旧版接口作为示例，生产环境需实现签名算法
	// 或参考: https://github.com/SocialSisterYi/bilibili-API-collect
	url := "https://api.bilibili.com/x/player/playurl"

	req, err := http.NewRequestWithContext(context.Background(), "GET", url, nil)
	if err != nil {
		return nil, err
	}

	q := req.URL.Query()
	q.Add("fnval", "4048")
	q.Add("bvid", bvid)
	q.Add("cid", cid)
	q.Add("qn", "116")
	req.URL.RawQuery = q.Encode()

	for k, v := range c.headers {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Code int `json:"code"`
		Data struct {
			Dash struct {
				Video []struct {
					BaseURL string `json:"baseUrl"`
					ID      int    `json:"id"`
					Width   int    `json:"width"`
					Height  int    `json:"height"` // 修复：小写
				} `json:"video"`
				Audio []struct {
					BaseURL string `json:"baseUrl"`
					ID      int    `json:"id"`
				} `json:"audio"`
				Flac struct {
					Audio struct {
						BaseURL string `json:"baseUrl"`
					} `json:"audio"`
				} `json:"flac"`
			} `json:"dash"`
		} `json:"data"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	if result.Code != 0 {
		return nil, fmt.Errorf("获取流地址失败: %s", result.Message)
	}

	if len(result.Data.Dash.Audio) == 0 {
		return nil, fmt.Errorf("未找到音频流")
	}
	if len(result.Data.Dash.Video) == 0 {
		return nil, fmt.Errorf("未找到视频流")
	}

	urls := &StreamURLs{
		AudioURL: result.Data.Dash.Audio[0].BaseURL,
		VideoURL: result.Data.Dash.Video[0].BaseURL,
	}

	// 优先使用Hi-Res音频
	if result.Data.Dash.Flac.Audio.BaseURL != "" {
		urls.AudioURL = result.Data.Dash.Flac.Audio.BaseURL
	}

	return urls, nil
}

// DownloadAndConvert 下载并转换单个视频
func (c *BilibiliClient) DownloadAndConvert(ctx context.Context, info VideoInfo, bvid string, outputDir string) error {
	maxRetries := 3
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		select {
		case <-ctx.Done():
			return fmt.Errorf("任务被取消: %w", ctx.Err())
		default:
		}

		fmt.Printf("[%d/%d] 正在处理: %s (第%d次尝试)\n", info.Index, info.PartName, attempt)

		// 1. 获取流地址
		urls, err := c.GetStreamURLs(bvid, info.CID)
		if err != nil {
			lastErr = err
			time.Sleep(time.Second * 2)
			continue
		}

		// 2. 下载音频
		audioPath := filepath.Join(outputDir, fmt.Sprintf("%s_P%d_%s_audio.mp3", bvid, info.Index, info.PartName))
		if err := c.downloadAudio(urls.AudioURL, audioPath); err != nil {
			lastErr = err
			continue
		}

		// 3. 按需下载视频
		if c.config.DownloadVideo {
			videoPath := filepath.Join(outputDir, fmt.Sprintf("%s_P%d_%s_video.mp4", bvid, info.Index, info.PartName))
			finalPath := filepath.Join(outputDir, fmt.Sprintf("%s_P%d_%s.mp4", bvid, info.Index, info.PartName))

			if err := c.downloadAndMergeVideo(urls.VideoURL, audioPath, videoPath, finalPath); err != nil {
				lastErr = err
				os.Remove(audioPath) // 清理临时文件
				continue
			}

			// 清理临时文件
			os.Remove(audioPath)
			os.Remove(videoPath)
			fmt.Printf("✓ 视频下载完成: %s\n", finalPath)
		} else {
			fmt.Printf("✓ 音频下载完成: %s\n", audioPath)
		}

		return nil // 成功则直接返回
	}

	return fmt.Errorf("重试%d次后失败: %w", maxRetries, lastErr)
}

// downloadAudio 下载音频流并转码为MP3
func (c *BilibiliClient) downloadAudio(audioURL, outputPath string) error {
	req, err := http.NewRequestWithContext(context.Background(), "GET", audioURL, nil)
	if err != nil {
		return err
	}

	for k, v := range c.headers {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("音频流返回状态码: %d", resp.StatusCode)
	}

	// 确保输出目录存在
	if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
		return err
	}

	cmd := exec.Command("ffmpeg",
		"-i", "pipe:0",
		"-q:a", "0", // 最高质量MP3
		"-f", "mp3",
		"-y", // 覆盖已存在文件
		outputPath,
	)
	cmd.Stdin = resp.Body
	cmd.Stderr = os.Stderr // 输出ffmpeg错误日志

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg转码失败: %w", err)
	}

	return nil
}

// downloadAndMergeVideo 下载视频流并与音频合并
func (c *BilibiliClient) downloadAndMergeVideo(videoURL, audioPath, videoTempPath, finalPath string) error {
	// 下载视频流
	req, err := http.NewRequestWithContext(context.Background(), "GET", videoURL, nil)
	if err != nil {
		return err
	}

	for k, v := range c.headers {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("视频流返回状态码: %d", resp.StatusCode)
	}

	// 直接合并音视频到最终文件
	cmd := exec.Command("ffmpeg",
		"-i", "pipe:0",
		"-i", audioPath,
		"-c:v", c.config.VideoEncoder,
		"-c:a", "copy", // 音频直接复制
		"-shortest",
		"-y",
		"-f", "mp4",
		finalPath,
	)
	cmd.Stdin = resp.Body
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("视频合并失败: %w", err)
	}

	return nil
}

// checkFFmpeg 检查ffmpeg是否可用
func checkFFmpeg() error {
	cmd := exec.Command("ffmpeg", "-version")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("未检测到ffmpeg，请确保已安装并添加到PATH")
	}
	return nil
}

// loadConfig 加载配置文件
func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	// 设置默认值
	if config.Concurrency <= 0 {
		config.Concurrency = 3 // 默认3个并发
	}
	if config.Timeout <= 0 {
		config.Timeout = 300 // 默认5分钟超时
	}
	if config.VideoEncoder == "" {
		config.VideoEncoder = "libx264"
	}

	return &config, nil
}

func main() {
	// 1. 检查依赖
	if err := checkFFmpeg(); err != nil {
		fmt.Fprintf(os.Stderr, "环境检查失败: %v\n", err)
		os.Exit(1)
	}

	// 2. 加载配置
	configPath := "./miaoConfig.json"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	config, err := loadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置加载失败: %v\n", err)
		os.Exit(1)
	}

	// 3. 创建客户端
	client := NewBilibiliClient(config)

	// 4. 输入BV号
	var bvid string
	fmt.Print("请输入B站视频BV号 (如: BV1xx411c7mD): ")
	fmt.Scanln(&bvid)
	bvid = strings.TrimSpace(bvid)
	if bvid == "" {
		fmt.Fprintln(os.Stderr, "BV号不能为空")
		os.Exit(1)
	}

	// 5. 获取视频信息
	fmt.Println("正在获取视频信息...")
	videoInfos, err := client.GetVideoInfo(bvid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "获取视频信息失败: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("共找到 %d 个分P\n", len(videoInfos))
	if config.DownloadVideo {
		fmt.Printf("下载模式: 视频+音频 (编码器: %s)\n", config.VideoEncoder)
	} else {
		fmt.Println("下载模式: 仅音频")
	}

	// 6. 创建下载目录
	outputDir := filepath.Join(".", "download", bvid)
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "创建目录失败: %v\n", err)
		os.Exit(1)
	}

	// 7. 并发下载（使用信号量控制）
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	errCh := make(chan error, len(videoInfos))
	semaphore := make(chan struct{}, config.Concurrency)

	// 处理中断信号
	go func() {
		os.Stdin.Read(make([]byte, 1)) // 按回车取消
		cancel()
	}()

	for _, info := range videoInfos {
		wg.Add(1)
		semaphore <- struct{}{} // 获取信号量

		go func(vi VideoInfo) {
			defer wg.Done()
			defer func() { <-semaphore }() // 释放信号量

			if err := client.DownloadAndConvert(ctx, vi, bvid, outputDir); err != nil {
				errCh <- fmt.Errorf("P%d %s: %w", vi.Index, vi.PartName, err)
			}
		}(info)
	}

	// 8. 等待完成并处理错误
	go func() {
		wg.Wait()
		close(errCh)
	}()

	errorCount := 0
	for err := range errCh {
		fmt.Fprintf(os.Stderr, "[错误] %v\n", err)
		errorCount++
	}

	// 9. 总结
	fmt.Printf("\n下载完成! 成功: %d, 失败: %d\n", len(videoInfos)-errorCount, errorCount)
	fmt.Printf("文件保存在: %s\n", outputDir)
}
