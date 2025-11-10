package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ==================== 类型定义（最前面） ====================

type Config struct {
	Cookie        string `json:"cookie"`
	CookieFile    string `json:"cookieFile"`
	DownloadVideo bool   `json:"downloadVideo"`
	VideoEncoder  string `json:"videoEncoder"`
	Concurrency   int    `json:"concurrency"`
	Timeout       int    `json:"timeout"`
}

type VideoInfo struct {
	CID      string
	PartName string
	Index    int
}

type StreamURLs struct {
	AudioURL string
	VideoURL string
	Codec    string
	Quality  int
}

type BilibiliClient struct {
	httpClient *http.Client
	headers    map[string]string
	config     *Config
}

// ==================== 构造函数和辅助函数 ====================

func NewBilibiliClient(config *Config) *BilibiliClient {
	return &BilibiliClient{
		httpClient: &http.Client{
			Timeout: time.Duration(config.Timeout) * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		},
		headers: getHeaders(config),
		config:  config,
	}
}

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

func sanitizeFileName(name string) string {
	reg := regexp.MustCompile(`[\x00-\x1f\x7f<>:"/\\|?*]+`)
	name = reg.ReplaceAllString(name, "_")
	if len(name) > 100 {
		name = name[:100]
	}
	return strings.TrimSpace(name)
}

// ==================== BilibiliClient 方法 ====================

func (c *BilibiliClient) GetVideoInfo(bvid string) ([]VideoInfo, error) {
	apiURL := fmt.Sprintf("https://api.bilibili.com/x/player/pagelist?bvid=%s", url.QueryEscape(bvid))
	
	req, err := http.NewRequestWithContext(context.Background(), "GET", apiURL, nil)
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
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API返回错误状态码: %d, body: %s", resp.StatusCode, string(body))
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

func (c *BilibiliClient) GetStreamURLs(bvid, cid string) (*StreamURLs, error) {
	apiURL := "https://api.bilibili.com/x/player/playurl"
	
	req, err := http.NewRequestWithContext(context.Background(), "GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	q := req.URL.Query()
	q.Add("fnval", "4048")
	q.Add("bvid", bvid)
	q.Add("cid", cid)
	q.Add("qn", "127")
	req.URL.RawQuery = q.Encode()

	for k, v := range c.headers {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API返回错误状态码: %d, body: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Code int `json:"code"`
		Data struct {
			Dash struct {
				Video []struct {
					BaseURL   string `json:"baseUrl"`
					ID        int    `json:"id"`
					Codecs    string `json:"codecs"`
					Width     int    `json:"width"`
					Height    int    `json:"height"`
					Bandwidth int    `json:"bandwidth"`
				} `json:"video"`
				Audio []struct {
					BaseURL   string `json:"baseUrl"`
					Bandwidth int    `json:"bandwidth"`
					Codecs    string `json:"codecs"`
				} `json:"audio"`
				Flac *struct {
					Audio *struct {
						BaseURL string `json:"baseUrl"`
					} `json:"audio"`
				} `json:"flac"`
			} `json:"dash"`
		} `json:"data"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("解析JSON失败: %w", err)
	}
	if result.Code != 0 {
		return nil, fmt.Errorf("获取流地址失败: %s", result.Message)
	}

	dash := result.Data.Dash
	if len(dash.Video) == 0 || len(dash.Audio) == 0 {
		return nil, fmt.Errorf("未找到视频或音频流")
	}

	bestVideoIndex := 0
	codecPriority := map[string]int{"avc": 3, "hev": 2, "av01": 1}
	
	for i, v := range dash.Video {
		currentCodec := strings.ToLower(v.Codecs)
		bestCodec := strings.ToLower(dash.Video[bestVideoIndex].Codecs)
		
		currentPriority := 0
		bestPriority := 0
		for codec, priority := range codecPriority {
			if strings.Contains(currentCodec, codec) {
				currentPriority = priority
			}
			if strings.Contains(bestCodec, codec) {
				bestPriority = priority
			}
		}
		
		if currentPriority > bestPriority {
			bestVideoIndex = i
		} else if currentPriority == bestPriority && v.ID > dash.Video[bestVideoIndex].ID {
			bestVideoIndex = i
		}
	}

	bestVideo := dash.Video[bestVideoIndex]

	var bestAudioURL string
	if dash.Flac != nil && dash.Flac.Audio != nil && dash.Flac.Audio.BaseURL != "" {
		bestAudioURL = dash.Flac.Audio.BaseURL
		fmt.Printf("  音频: FLAC无损\n")
	} else {
		bestAudioIndex := 0
		for i, a := range dash.Audio {
			if a.Bandwidth >= 180000 && a.Bandwidth <= 200000 {
				bestAudioIndex = i
				break
			}
			if a.Bandwidth > dash.Audio[bestAudioIndex].Bandwidth {
				bestAudioIndex = i
			}
		}
		bestAudioURL = dash.Audio[bestAudioIndex].BaseURL
		fmt.Printf("  音频: %dkbps %s\n", dash.Audio[bestAudioIndex].Bandwidth/1000, dash.Audio[bestAudioIndex].Codecs)
	}

	fmt.Printf("  视频: %s, %dx%d, %dkbps\n", 
		bestVideo.Codecs, bestVideo.Width, bestVideo.Height, bestVideo.Bandwidth/1000)
	
	return &StreamURLs{
		VideoURL: bestVideo.BaseURL,
		AudioURL: bestAudioURL,
		Codec:    bestVideo.Codecs,
		Quality:  bestVideo.Height,
	}, nil
}

func (c *BilibiliClient) DownloadAndConvert(ctx context.Context, info VideoInfo, bvid string, outputDir string) error {
	maxRetries := 3
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		select {
		case <-ctx.Done():
			return fmt.Errorf("任务被取消: %w", ctx.Err())
		default:
		}

		fmt.Printf("[%d/%s] 正在处理: %s (尝试 %d/%d)\n", 
			info.Index, bvid, info.PartName, attempt, maxRetries)

		urls, err := c.GetStreamURLs(bvid, info.CID)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}

		tempBase := filepath.Join(outputDir, fmt.Sprintf(".tmp_%d_%s", info.Index, 
			strings.TrimPrefix(info.PartName, ".")))
		
		audioTempPath := tempBase + "_audio.m4a"
		videoTempPath := tempBase + "_video.mp4"
		
		if err := c.downloadStreamWithRetry(urls.AudioURL, audioTempPath, "音频"); err != nil {
			lastErr = err
			cleanupFiles(audioTempPath, videoTempPath)
			continue
		}

		if c.config.DownloadVideo {
			finalPath := filepath.Join(outputDir, fmt.Sprintf("%d.%s.mp4", info.Index, info.PartName))
			
			if err := c.downloadStreamWithRetry(urls.VideoURL, videoTempPath, "视频"); err != nil {
				lastErr = err
				cleanupFiles(audioTempPath, videoTempPath)
				continue
			}

			fmt.Printf("  正在合并音视频...\n")
			if err := c.mergeVideoAudio(videoTempPath, audioTempPath, finalPath); err != nil {
				lastErr = err
				cleanupFiles(audioTempPath, videoTempPath)
				continue
			}

			cleanupFiles(audioTempPath, videoTempPath)
			fmt.Printf("✓ 视频完成: %s\n", filepath.Base(finalPath))
		} else {
			finalAudioPath := filepath.Join(outputDir, fmt.Sprintf("%d.%s.mp3", info.Index, info.PartName))
			fmt.Printf("  正在转码为MP3...\n")
			if err := c.convertToMP3(audioTempPath, finalAudioPath); err != nil {
				lastErr = err
				cleanupFiles(audioTempPath)
				continue
			}
			cleanupFiles(audioTempPath)
			fmt.Printf("✓ 音频完成: %s\n", filepath.Base(finalAudioPath))
		}

		return nil
	}

	return fmt.Errorf("重试%d次后失败: %w", maxRetries, lastErr)
}

func (c *BilibiliClient) mergeVideoAudio(videoPath, audioPath, finalPath string) error {
	cmd := exec.Command("ffmpeg",
		"-i", videoPath,
		"-i", audioPath,
		"-c:v", "copy",
		"-c:a", "copy",
		"-shortest",
		"-movflags", "+faststart",
		"-y",
		finalPath,
	)
	cmd.Stderr = os.Stderr
	
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg合并失败: %w", err)
	}
	return nil
}

func (c *BilibiliClient) convertToMP3(inputPath, outputPath string) error {
	cmd := exec.Command("ffmpeg",
		"-i", inputPath,
		"-c:a", "libmp3lame",
		"-q:a", "0",
		"-y",
		outputPath,
	)
	cmd.Stderr = os.Stderr
	
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg转码失败: %w", err)
	}
	return nil
}


func (c *BilibiliClient) downloadStreamWithRetry(streamURL, outputPath, streamType string) error {
	var lastErr error
	for i := 0; i < 3; i++ {
		if err := c.downloadStream(streamURL, outputPath); err != nil {
			lastErr = err
			time.Sleep(time.Duration(i+1) * time.Second)
			continue
		}
		return nil
	}
	return fmt.Errorf("%s下载失败: %w", streamType, lastErr)
}

func (c *BilibiliClient) downloadStream(streamURL, outputPath string) error {
	req, err := http.NewRequestWithContext(context.Background(), "GET", streamURL, nil)
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
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("返回状态码: %d, body: %s", resp.StatusCode, string(body))
	}

	if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
		return err
	}

	out, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, resp.Body); err != nil {
		return err
	}

	return nil
}

// ==================== 独立辅助函数 ====================

func parsePartSelection(input string, max int) ([]int, error) {
	input = strings.TrimSpace(strings.ToLower(input))
	
	if input == "" || input == "all" {
		parts := make([]int, max)
		for i := 0; i < max; i++ {
			parts[i] = i + 1
		}
		return parts, nil
	}

	selected := make(map[int]bool)
	parts := strings.Split(input, ",")
	
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if strings.Contains(part, "-") {
			rangeParts := strings.Split(part, "-")
			if len(rangeParts) != 2 {
				return nil, fmt.Errorf("无效的格式: %s", part)
			}
			
			start, err := strconv.Atoi(strings.TrimSpace(rangeParts[0]))
			if err != nil {
				return nil, fmt.Errorf("无效的数字: %s", rangeParts[0])
			}
			
			end, err := strconv.Atoi(strings.TrimSpace(rangeParts[1]))
			if err != nil {
				return nil, fmt.Errorf("无效的数字: %s", rangeParts[1])
			}
			
			if start < 1 || end > max || start > end {
				return nil, fmt.Errorf("无效的范围: %d-%d (有效范围: 1-%d)", start, end, max)
			}
			
			for i := start; i <= end; i++ {
				selected[i] = true
			}
		} else {
			num, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("无效的数字: %s", part)
			}
			
			if num < 1 || num > max {
				return nil, fmt.Errorf("分P编号 %d 超出范围 (有效范围: 1-%d)", num, max)
			}
			
			selected[num] = true
		}
	}

	result := make([]int, 0, len(selected))
	for num := range selected {
		result = append(result, num)
	}
	
	return result, nil
}

func mergeVideoAudio(videoPath, audioPath, finalPath string) error {
	cmd := exec.Command("ffmpeg",
		"-i", videoPath,
		"-i", audioPath,
		"-c:v", "copy",
		"-c:a", "copy",
		"-shortest",
		"-movflags", "+faststart",
		"-y",
		finalPath,
	)
	cmd.Stderr = os.Stderr
	
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg合并失败: %w", err)
	}
	return nil
}

func convertToMP3(inputPath, outputPath string) error {
	cmd := exec.Command("ffmpeg",
		"-i", inputPath,
		"-c:a", "libmp3lame",
		"-q:a", "0",
		"-y",
		outputPath,
	)
	cmd.Stderr = os.Stderr
	
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg转码失败: %w", err)
	}
	return nil
}

func cleanupFiles(paths ...string) {
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "清理临时文件失败 %s: %v\n", p, err)
		}
	}
}

func checkFFmpeg() error {
	cmd := exec.Command("ffmpeg", "-version")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("未检测到ffmpeg，请确保已安装并添加到PATH")
	}
	return nil
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{
				Concurrency:  3,
				Timeout:      300,
				VideoEncoder: "libx264",
			}, nil
		}
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	if config.Concurrency <= 0 {
		config.Concurrency = 3
	}
	if config.Timeout <= 0 {
		config.Timeout = 300
	}
	if config.VideoEncoder == "" {
		config.VideoEncoder = "libx264"
	}

	return &config, nil
}

func GetBvidFromURL(videoURL string) (string, error) {
	videoURL = strings.TrimSpace(videoURL)
	
	if strings.HasPrefix(videoURL, "BV") && len(videoURL) == 12 {
		return videoURL, nil
	}

	if strings.Contains(videoURL, "b23.tv") || strings.Contains(videoURL, "bili2233.cn") {
		realURL, err := followRedirect(videoURL)
		if err != nil {
			return "", fmt.Errorf("短链接解析失败: %v", err)
		}
		videoURL = realURL
	}

	re := regexp.MustCompile(`BV[0-9a-zA-Z]{10}`)
	matches := re.FindStringSubmatch(videoURL)
	if len(matches) == 0 {
		return "", fmt.Errorf("未找到有效的BV号")
	}
	return matches[0], nil
}

func followRedirect(shortURL string) (string, error) {
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Get(shortURL)
	if err != nil {
		return "", fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		if location := resp.Header.Get("Location"); location != "" {
			return location, nil
		}
	}
	return shortURL, nil
}

func loadCookie(config *Config, cookieFile string) error {
	if cookieFile != "" {
		data, err := os.ReadFile(cookieFile)
		if err != nil {
			return fmt.Errorf("读取Cookie文件失败: %w", err)
		}
		config.Cookie = strings.TrimSpace(string(data))
		fmt.Printf("✓ 已从文件加载Cookie: %s\n", cookieFile)
		return nil
	}

	if envCookie := os.Getenv("BILIBILI_COOKIE"); envCookie != "" {
		config.Cookie = envCookie
		fmt.Println("✓ 已从环境变量加载Cookie")
		return nil
	}

	if config.Cookie != "" {
		fmt.Println("✓ 已从配置文件加载Cookie")
		return nil
	}

	fmt.Println("⚠ 警告：未配置Cookie，可能无法下载部分视频")
	return nil
}

// ==================== main函数（最后） ====================

func main() {
	cookieFile := flag.String("cookie-file", "", "指定Cookie文件路径")
	downloadDir := flag.String("output", "./download", "指定输出目录")
	flag.Parse()

	if err := checkFFmpeg(); err != nil {
		fmt.Fprintf(os.Stderr, "环境检查失败: %v\n", err)
		os.Exit(1)
	}

	configPath := "./miaoConfig.json"
	if flag.NArg() > 0 {
		configPath = flag.Arg(0)
	}

	config, err := loadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置加载失败: %v\n", err)
		os.Exit(1)
	}

	if err := loadCookie(config, *cookieFile); err != nil {
		fmt.Fprintf(os.Stderr, "Cookie加载失败: %v\n", err)
		os.Exit(1)
	}

	client := NewBilibiliClient(config)

	fmt.Print("请输入B站视频链接或BV号: ")
	var input string
	fmt.Scanln(&input)

	bvid, err := GetBvidFromURL(input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "解析失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("解析成功，BV号: %s\n", bvid)

	fmt.Println("正在获取视频信息...")
	videoInfos, err := client.GetVideoInfo(bvid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "获取视频信息失败: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("共找到 %d 个分P\n", len(videoInfos))
	for i, info := range videoInfos {
		fmt.Printf("  [%d] %s\n", i+1, info.PartName)
	}
	if config.DownloadVideo {
		fmt.Printf("下载模式: 视频+音频 (编码器: %s)\n", config.VideoEncoder)
	} else {
		fmt.Println("下载模式: 仅音频")
	}

	var selectedIndices []int
	if len(videoInfos) > 1 {
		fmt.Print("\n请输入要下载的分P (all=全部, 1=第一集, 1-3=1到3集, 1,3,5=第1,3,5集): ")
		var selection string
		fmt.Scanln(&selection)
		
		selectedIndices, err = parsePartSelection(selection, len(videoInfos))
		if err != nil {
			fmt.Fprintf(os.Stderr, "输入无效: %v\n", err)
			os.Exit(1)
		}
	} else {
		selectedIndices = []int{1}
	}

	selectedInfos := make([]VideoInfo, 0)
	for _, info := range videoInfos {
		for _, idx := range selectedIndices {
			if info.Index == idx {
				selectedInfos = append(selectedInfos, info)
				break
			}
		}
	}
	fmt.Printf("已选择下载 %d 个分P\n\n", len(selectedInfos))

	outputDir := filepath.Join(*downloadDir, bvid)
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "创建目录失败: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\n接收到中断信号，正在取消任务...")
		cancel()
	}()

	var wg sync.WaitGroup
	errCh := make(chan error, len(selectedInfos))
	semaphore := make(chan struct{}, config.Concurrency)

	for _, info := range selectedInfos {
		wg.Add(1)
		semaphore <- struct{}{}

		go func(vi VideoInfo) {
			defer wg.Done()
			defer func() { <-semaphore }()

			if err := client.DownloadAndConvert(ctx, vi, bvid, outputDir); err != nil {
				errCh <- fmt.Errorf("P%d %s: %w", vi.Index, vi.PartName, err)
			}
		}(info)
	}

	go func() {
		wg.Wait()
		close(errCh)
	}()

	errorCount := 0
	for err := range errCh {
		fmt.Fprintf(os.Stderr, "[错误] %v\n", err)
		errorCount++
	}

	fmt.Printf("\n下载完成! 成功: %d, 失败: %d\n", len(selectedInfos)-errorCount, errorCount)
	fmt.Printf("文件保存在: %s\n", outputDir)
}
