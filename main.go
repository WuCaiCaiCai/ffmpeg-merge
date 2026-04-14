package main

import (
	"bufio"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ── Global state ────────────────────────────────────────────────────────────

var (
	inputFiles      []string
	durationsMS     []int64
	hwEncoders      []string
	encodeArgs      []string
	totalMS         int64
	useGPU          bool
	qualityMode     int
	selectedEncoder string
	vaapiDevice     string
	inputDir        string
	outputFile      string
)

// ── Shared stdin reader (must be single instance to avoid losing buffered bytes) ──

var stdinReader = bufio.NewReader(os.Stdin)

// ── Helpers ─────────────────────────────────────────────────────────────────

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "错误: "+format+"\n", a...)
	os.Exit(1)
}

func warn(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "警告: "+format+"\n", a...)
}

func info(format string, a ...any) {
	fmt.Printf(format+"\n", a...)
}

func formatHMS(sec int64) string {
	if sec < 0 {
		sec = 0
	}
	return fmt.Sprintf("%02d:%02d:%02d", sec/3600, (sec%3600)/60, sec%60)
}

func escapeFFConcatPath(input string) string {
	return strings.ReplaceAll(input, "'", `'\\''`)
}

func escapeMetadataText(text string) string {
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.ReplaceAll(text, `\`, `\\`)
	text = strings.ReplaceAll(text, "=", `\=`)
	text = strings.ReplaceAll(text, ";", `\;`)
	text = strings.ReplaceAll(text, "#", `\#`)
	return text
}

func askYesNo(prompt string, defaultYes bool) bool {
	suffix := "[Y/n]"
	if !defaultYes {
		suffix = "[y/N]"
	}
	fmt.Printf("%s %s: ", prompt, suffix)
	line, _ := stdinReader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return defaultYes
	}
	return strings.EqualFold(line, "y")
}

func readLine(prompt string) string {
	fmt.Print(prompt)
	line, _ := stdinReader.ReadString('\n')
	return strings.TrimSpace(line)
}

// ── Tool checks ─────────────────────────────────────────────────────────────

func requireTools() {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		die("未找到 ffmpeg，请先安装。")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		die("未找到 ffprobe，请先安装。")
	}
}

// ── Video extensions ────────────────────────────────────────────────────────

var videoExts = map[string]bool{
	".mp4": true, ".mkv": true, ".mov": true,
	".m4v": true, ".ts": true, ".webm": true,
}

// ── Natural sort ────────────────────────────────────────────────────────────

// naturalSortKey splits a string into a sequence of (text, number) chunks
// for natural ordering: "#1 foo" < "#2 foo" < "#10 foo".
type sortChunk struct {
	text  string // lowercased text portion (empty if this chunk is purely numeric)
	num   int64  // numeric value (0 if text-only)
	isNum bool
}

func parseSortChunks(s string) []sortChunk {
	var chunks []sortChunk
	i := 0
	for i < len(s) {
		if s[i] >= '0' && s[i] <= '9' {
			j := i
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			n, _ := strconv.ParseInt(s[i:j], 10, 64)
			chunks = append(chunks, sortChunk{num: n, isNum: true})
			i = j
		} else {
			j := i
			for j < len(s) && !(s[j] >= '0' && s[j] <= '9') {
				j++
			}
			chunks = append(chunks, sortChunk{text: strings.ToLower(s[i:j])})
			i = j
		}
	}
	return chunks
}

func naturalLess(a, b string) bool {
	ca := parseSortChunks(a)
	cb := parseSortChunks(b)
	for i := 0; i < len(ca) && i < len(cb); i++ {
		ai, bi := ca[i], cb[i]
		if ai.isNum != bi.isNum {
			// numbers sort before text
			return ai.isNum
		}
		if ai.isNum {
			if ai.num != bi.num {
				return ai.num < bi.num
			}
		} else {
			if ai.text != bi.text {
				return ai.text < bi.text
			}
		}
	}
	return len(ca) < len(cb)
}

// ── Collect input files ─────────────────────────────────────────────────────

func collectInputFiles(outputAbs string) {
	entries, err := os.ReadDir(inputDir)
	if err != nil {
		die("无法读取目录: %s", inputDir)
	}

	inputFiles = nil
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if !videoExts[ext] {
			continue
		}
		fullPath := filepath.Join(inputDir, e.Name())
		abs, _ := filepath.Abs(fullPath)
		// Exclude the output file from input list (fixes EBML bug)
		if abs == outputAbs {
			continue
		}
		inputFiles = append(inputFiles, abs)
	}

	sort.Slice(inputFiles, func(i, j int) bool {
		return naturalLess(filepath.Base(inputFiles[i]), filepath.Base(inputFiles[j]))
	})

	if len(inputFiles) == 0 {
		die("目录内未发现可合并视频文件。")
	}
}

// ── Probe duration ──────────────────────────────────────────────────────────

func probeDurationMS(file string) int64 {
	out, err := exec.Command("ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=nw=1:nk=1",
		file,
	).Output()
	if err != nil {
		return 0
	}
	s := strings.TrimSpace(string(out))
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return 0
	}
	return int64(math.Round(f * 1000))
}

func probeTotalDuration() {
	durationsMS = nil
	totalMS = 0
	for _, file := range inputFiles {
		d := probeDurationMS(file)
		durationsMS = append(durationsMS, d)
		totalMS += d
	}
}

// ── System info ─────────────────────────────────────────────────────────────

func showSystemInfo() {
	info("4) 系统信息与硬件能力检测")

	verOut, _ := exec.Command("ffmpeg", "-version").Output()
	if lines := strings.SplitN(string(verOut), "\n", 2); len(lines) > 0 {
		info("   - %s", strings.TrimSpace(lines[0]))
	}
	info("   - 系统: %s/%s", runtime.GOOS, runtime.GOARCH)

	hwOut, _ := exec.Command("ffmpeg", "-hide_banner", "-hwaccels").Output()
	lines := strings.Split(strings.TrimSpace(string(hwOut)), "\n")
	if len(lines) > 1 {
		var accels []string
		for _, l := range lines[1:] {
			l = strings.TrimSpace(l)
			if l != "" {
				accels = append(accels, l)
			}
		}
		if len(accels) > 0 {
			info("   - 硬件加速: %s", strings.Join(accels, " "))
		} else {
			info("   - 硬件加速: 未检测到")
		}
	} else {
		info("   - 硬件加速: 未检测到")
	}
}

// ── GPU / encoder detection ─────────────────────────────────────────────────

// testEncoder tries a minimal encode to verify the encoder actually works at runtime.
// Returns true if the encoder can produce output (driver/library present).
func testEncoder(encoder string) bool {
	// Use 256x256 — many HW encoders have a minimum of 128x128.
	args := []string{
		"-hide_banner", "-nostats", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=black:s=256x256:d=0.1:r=1",
		"-frames:v", "1",
	}

	// VAAPI needs a device for the test
	if strings.HasSuffix(encoder, "vaapi") {
		matches, _ := filepath.Glob("/dev/dri/renderD*")
		if len(matches) == 0 {
			return false
		}
		sort.Strings(matches)
		args = append(args,
			"-init_hw_device", fmt.Sprintf("vaapi=va:%s", matches[0]),
			"-filter_hw_device", "va",
			"-vf", "format=nv12,hwupload",
		)
	}

	args = append(args, "-c:v", encoder, "-f", "null", "-")
	cmd := exec.Command("ffmpeg", args...)
	return cmd.Run() == nil
}

func detectHWEncoders() {
	hwEncoders = nil
	out, err := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output()
	if err != nil {
		return
	}
	encodersOutput := string(out)

	candidates := []string{
		// VAAPI
		"av1_vaapi", "hevc_vaapi", "h264_vaapi",
		// NVENC
		"av1_nvenc", "hevc_nvenc", "h264_nvenc",
		// QSV
		"av1_qsv", "hevc_qsv", "h264_qsv",
	}
	if runtime.GOOS == "darwin" {
		candidates = append(candidates, "h264_videotoolbox", "hevc_videotoolbox")
	}
	if runtime.GOOS == "windows" {
		candidates = append(candidates, "h264_amf", "hevc_amf")
	}

	// First pass: filter by compile-time availability
	var compiled []string
	for _, enc := range candidates {
		pattern := regexp.MustCompile(`\s` + regexp.QuoteMeta(enc) + `(\s|$)`)
		if pattern.MatchString(encodersOutput) {
			compiled = append(compiled, enc)
		}
	}

	if len(compiled) == 0 {
		return
	}

	// Second pass: verify each encoder actually works at runtime
	info("   正在验证硬件编码器可用性...")
	for _, enc := range compiled {
		if testEncoder(enc) {
			hwEncoders = append(hwEncoders, enc)
		} else {
			warn("编码器 %s 已编译但运行时不可用（缺少驱动/库），已跳过", enc)
		}
	}
}

func chooseVAAPIDeviceIfNeeded() {
	if !strings.HasSuffix(selectedEncoder, "vaapi") {
		return
	}

	matches, _ := filepath.Glob("/dev/dri/renderD*")
	if len(matches) == 0 {
		warn("检测到 VAAPI 编码器，但未找到 /dev/dri/renderD* 设备，自动回退到软件编码。")
		useGPU = false
		selectedEncoder = ""
		vaapiDevice = ""
		return
	}

	sort.Strings(matches)

	if len(matches) == 1 {
		vaapiDevice = matches[0]
		info("   - VAAPI 设备: %s", vaapiDevice)
		return
	}

	info("   - 检测到多个 VAAPI 设备，请选择：")
	for i, node := range matches {
		fmt.Printf("     %d) %s\n", i+1, node)
	}
	for {
		s := readLine("请选择 VAAPI 设备编号（默认 1）: ")
		if s == "" {
			s = "1"
		}
		n, err := strconv.Atoi(s)
		if err == nil && n >= 1 && n <= len(matches) {
			vaapiDevice = matches[n-1]
			break
		}
		warn("请输入有效编号。")
	}
}

func chooseGPUAndEncoder() {
	if !askYesNo("   是否使用显卡加速（用于轻度/重度压缩）?", true) {
		useGPU = false
		selectedEncoder = ""
		return
	}

	detectHWEncoders()
	if len(hwEncoders) == 0 {
		warn("未检测到可用硬件编码器，将使用 CPU 软件编码。")
		useGPU = false
		selectedEncoder = ""
		return
	}

	useGPU = true
	info("   可用硬件编码器：")
	for i, enc := range hwEncoders {
		fmt.Printf("     %d) %s\n", i+1, enc)
	}
	cpuIdx := len(hwEncoders) + 1
	fmt.Printf("     %d) 软件编码（CPU）\n", cpuIdx)

	for {
		s := readLine("请选择编码器编号（默认 1）: ")
		if s == "" {
			s = "1"
		}
		n, err := strconv.Atoi(s)
		if err == nil && n >= 1 && n <= cpuIdx {
			if n == cpuIdx {
				useGPU = false
				selectedEncoder = ""
			} else {
				selectedEncoder = hwEncoders[n-1]
			}
			break
		}
		warn("请输入有效编号。")
	}

	chooseVAAPIDeviceIfNeeded()
}

// ── Quality mode ────────────────────────────────────────────────────────────

func chooseQualityMode() {
	info("5) 请选择压缩质量/速度：")
	info("   1) 快速无损直接合并（stream copy，最快）")
	info("   2) 轻度压缩（质量/体积平衡）")
	info("   3) 重度压缩（优先体积）")

	for {
		s := readLine("请输入 1/2/3（默认 1）: ")
		if s == "" {
			s = "1"
		}
		if s == "1" || s == "2" || s == "3" {
			qualityMode, _ = strconv.Atoi(s)
			break
		}
		warn("请输入 1、2 或 3。")
	}
}

// ── Stream compatibility check ──────────────────────────────────────────────

func streamSignature(file string) string {
	video, _ := exec.Command("ffprobe",
		"-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=codec_name,width,height,pix_fmt,r_frame_rate",
		"-of", "csv=p=0", file,
	).Output()
	audio, _ := exec.Command("ffprobe",
		"-v", "error", "-select_streams", "a:0",
		"-show_entries", "stream=codec_name,channels,sample_rate",
		"-of", "csv=p=0", file,
	).Output()
	return strings.TrimSpace(string(video)) + "|" + strings.TrimSpace(string(audio))
}

func ensureCopyModeCompatibility() {
	if qualityMode != 1 || len(inputFiles) < 2 {
		return
	}
	ref := streamSignature(inputFiles[0])
	for _, file := range inputFiles[1:] {
		sig := streamSignature(file)
		if sig != ref {
			warn("检测到文件参数不一致，快速无损直拷贝可能失败。")
			if askYesNo("是否自动切换到 2) 轻度压缩?", true) {
				qualityMode = 2
			}
			break
		}
	}
}

// ── Build concat list & metadata ────────────────────────────────────────────

func buildConcatList(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, file := range inputFiles {
		escaped := escapeFFConcatPath(file)
		fmt.Fprintf(f, "file '%s'\n", escaped)
	}
	return nil
}

func buildMetadata(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprintln(f, ";FFMETADATA1")

	var start int64
	for i, file := range inputFiles {
		dur := durationsMS[i]
		end := start + dur
		base := filepath.Base(file)
		title := strings.TrimSuffix(base, filepath.Ext(base))
		title = escapeMetadataText(title)

		fmt.Fprintln(f, "[CHAPTER]")
		fmt.Fprintln(f, "TIMEBASE=1/1000")
		fmt.Fprintf(f, "START=%d\n", start)
		fmt.Fprintf(f, "END=%d\n", end)
		fmt.Fprintf(f, "title=%s\n", title)
		start = end
	}
	return nil
}

// ── Encode args ─────────────────────────────────────────────────────────────

func setEncodeArgs() {
	encodeArgs = nil

	if qualityMode == 1 {
		encodeArgs = []string{"-c", "copy"}
		return
	}

	if useGPU && selectedEncoder != "" {
		switch {
		case strings.HasSuffix(selectedEncoder, "vaapi"):
			if vaapiDevice == "" {
				die("VAAPI 设备未设置。")
			}
			encodeArgs = append(encodeArgs,
				"-init_hw_device", fmt.Sprintf("vaapi=va:%s", vaapiDevice),
				"-filter_hw_device", "va",
				"-vf", "format=nv12,hwupload",
				"-c:v", selectedEncoder,
			)
			if qualityMode == 2 {
				encodeArgs = append(encodeArgs, "-global_quality", "23")
			} else {
				encodeArgs = append(encodeArgs, "-global_quality", "31")
			}

		case strings.HasSuffix(selectedEncoder, "nvenc"):
			encodeArgs = append(encodeArgs, "-c:v", selectedEncoder)
			if qualityMode == 2 {
				encodeArgs = append(encodeArgs, "-preset", "p5", "-cq", "23", "-b:v", "0")
			} else {
				encodeArgs = append(encodeArgs, "-preset", "p7", "-cq", "31", "-b:v", "0")
			}

		case strings.HasSuffix(selectedEncoder, "qsv"):
			encodeArgs = append(encodeArgs, "-c:v", selectedEncoder)
			if qualityMode == 2 {
				encodeArgs = append(encodeArgs, "-global_quality", "23")
			} else {
				encodeArgs = append(encodeArgs, "-global_quality", "31")
			}

		case strings.HasSuffix(selectedEncoder, "videotoolbox"):
			encodeArgs = append(encodeArgs, "-c:v", selectedEncoder)
			if qualityMode == 2 {
				encodeArgs = append(encodeArgs, "-q:v", "65")
			} else {
				encodeArgs = append(encodeArgs, "-q:v", "45")
			}

		case strings.HasSuffix(selectedEncoder, "amf"):
			encodeArgs = append(encodeArgs, "-c:v", selectedEncoder)
			if qualityMode == 2 {
				encodeArgs = append(encodeArgs, "-quality", "balanced", "-rc", "cqp", "-qp_i", "23", "-qp_p", "23")
			} else {
				encodeArgs = append(encodeArgs, "-quality", "quality", "-rc", "cqp", "-qp_i", "31", "-qp_p", "31")
			}

		default:
			// Unknown GPU encoder, fall back to CPU
			encodeArgs = append(encodeArgs, "-c:v", "libx264", "-preset", "medium", "-crf", "20")
		}
	} else {
		// CPU software encoding
		if qualityMode == 2 {
			encodeArgs = append(encodeArgs, "-c:v", "libx264", "-preset", "medium", "-crf", "20")
		} else {
			encodeArgs = append(encodeArgs, "-c:v", "libx265", "-preset", "slow", "-crf", "30")
		}
	}

	// Audio encoding for re-encode modes
	if qualityMode == 2 {
		encodeArgs = append(encodeArgs, "-c:a", "aac", "-b:a", "160k")
	} else {
		encodeArgs = append(encodeArgs, "-c:a", "aac", "-b:a", "128k")
	}
	encodeArgs = append(encodeArgs, "-c:s", "copy", "-movflags", "+faststart")
}

// ── Progress rendering ──────────────────────────────────────────────────────

func renderProgress(scanner *bufio.Scanner, totalMS int64, startTime time.Time) {
	var currentMS int64
	var lastMS int64 = -1

	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key, value := parts[0], parts[1]

		switch key {
		case "out_time_ms", "out_time_us":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				continue
			}
			currentMS = n / 1000
			if currentMS == lastMS {
				continue
			}
			lastMS = currentMS
			if totalMS > 0 && currentMS > 0 {
				elapsed := time.Since(startTime).Seconds()
				percent := float64(currentMS) / float64(totalMS) * 100
				var etaSec float64
				if currentMS > 0 {
					etaSec = elapsed * float64(totalMS-currentMS) / float64(currentMS)
				}
				fmt.Printf("\r7) 合并中: %6.2f%% | 已用 %s | 预计剩余 %s",
					percent,
					formatHMS(int64(elapsed)),
					formatHMS(int64(etaSec)),
				)
			}
		case "progress":
			if value == "end" {
				return
			}
		}
	}
}

// ── Run merge ───────────────────────────────────────────────────────────────

func runMerge(concatFile, metaFile, logFile string) {
	args := []string{
		"-hide_banner", "-y",
		"-f", "concat", "-safe", "0", "-i", concatFile,
		"-i", metaFile,
		"-map", "0:v?", "-map", "0:a?", "-map", "0:s?",
		"-map_metadata", "1", "-map_chapters", "1",
	}
	args = append(args, encodeArgs...)
	args = append(args, outputFile)
	args = append(args, "-progress", "pipe:1", "-nostats")

	logF, err := os.Create(logFile)
	if err != nil {
		die("无法创建日志文件: %v", err)
	}

	cmd := exec.Command("ffmpeg", args...)
	cmd.Stderr = logF

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		logF.Close()
		die("无法创建管道: %v", err)
	}

	startTime := time.Now()
	if err := cmd.Start(); err != nil {
		logF.Close()
		die("启动 ffmpeg 失败: %v", err)
	}

	scanner := bufio.NewScanner(stdout)
	renderProgress(scanner, totalMS, startTime)

	err = cmd.Wait()
	logF.Close()
	fmt.Println()

	if err != nil {
		warn("合并失败，ffmpeg 日志摘录：")
		logContent, readErr := os.ReadFile(logFile)
		if readErr == nil {
			lines := strings.Split(string(logContent), "\n")
			start := 0
			if len(lines) > 30 {
				start = len(lines) - 30
			}
			for _, l := range lines[start:] {
				fmt.Fprintln(os.Stderr, l)
			}
		}
		if qualityMode == 1 {
			warn("建议改用 2) 轻度压缩，可兼容参数不完全一致的视频。")
		}
		os.Exit(1)
	}
}

// ── Output summary ──────────────────────────────────────────────────────────

func printOutputSummary() {
	var sizeBytes int64
	if st, err := os.Stat(outputFile); err == nil {
		sizeBytes = st.Size()
	}

	durationOut, _ := exec.Command("ffprobe",
		"-v", "error", "-show_entries", "format=duration",
		"-of", "default=nw=1:nk=1", outputFile,
	).Output()
	duration, _ := strconv.ParseFloat(strings.TrimSpace(string(durationOut)), 64)

	codecOut, _ := exec.Command("ffprobe",
		"-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=codec_name",
		"-of", "default=nw=1:nk=1", outputFile,
	).Output()
	codec := strings.TrimSpace(string(codecOut))
	if codec == "" {
		codec = "unknown"
	}

	chaptersOut, _ := exec.Command("ffprobe",
		"-v", "error", "-show_entries", "chapter=id",
		"-of", "csv=p=0", outputFile,
	).Output()
	chapters := 0
	for _, l := range strings.Split(strings.TrimSpace(string(chaptersOut)), "\n") {
		if strings.TrimSpace(l) != "" {
			chapters++
		}
	}

	info("8) 合并完成")
	info("   - 输出文件: %s", outputFile)
	info("   - 文件大小: %.2f MB", float64(sizeBytes)/1024/1024)
	if duration > 0 {
		info("   - 时长: %s", formatHMS(int64(duration)))
	} else {
		info("   - 时长: 00:00:00")
	}
	info("   - 视频编码: %s", codec)
	info("   - 章节数: %d", chapters)
}

// ── Interactive mode ────────────────────────────────────────────────────────

func runInteractive() {
	info("1) 启动程序：FFmpeg 智能合并工具")

	// Ask input dir
	cwd, _ := os.Getwd()
	dir := readLine(fmt.Sprintf("2) 请输入待合并文件目录（默认当前目录）: "))
	if dir == "" {
		dir = cwd
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		die("无效路径: %s", dir)
	}
	st, err := os.Stat(absDir)
	if err != nil || !st.IsDir() {
		die("目录不存在: %s", absDir)
	}
	inputDir = absDir

	// We need a preliminary output path to exclude from inputs.
	// First collect without exclusion, then ask output, then re-collect.
	collectInputFiles("") // initial collect, no exclusion

	// Show files
	info("3) 已按名称排序（包含 # 的文件名可正常处理）：")
	for i, file := range inputFiles {
		fmt.Printf("   %2d. %s\n", i+1, filepath.Base(file))
	}

	probeTotalDuration()
	showSystemInfo()
	chooseGPUAndEncoder()
	chooseQualityMode()
	ensureCopyModeCompatibility()

	// Ask output path — default to CWD (fixes EBML bug)
	defaultName := fmt.Sprintf("merged_%s.mp4", time.Now().Format("20060102_150405"))
	defaultPath := filepath.Join(cwd, defaultName)
	input := readLine(fmt.Sprintf("6) 请输入输出路径（可填目录，默认 %s）: ", defaultPath))
	if input == "" {
		input = defaultPath
	}
	if st, err := os.Stat(input); err == nil && st.IsDir() {
		outputFile = filepath.Join(input, defaultName)
	} else {
		outputFile = input
		if filepath.Ext(outputFile) == "" {
			outputFile += ".mp4"
		}
	}
	outputFile, _ = filepath.Abs(outputFile)
	if err := os.MkdirAll(filepath.Dir(outputFile), 0o755); err != nil {
		die("无法创建输出目录: %v", err)
	}

	// Re-collect with output exclusion
	collectInputFiles(outputFile)
	if len(inputFiles) == 0 {
		die("目录内未发现可合并视频文件。")
	}

	// Re-probe durations after exclusion
	probeTotalDuration()

	finishMerge()
}

// ── CLI mode ────────────────────────────────────────────────────────────────

func runCLI(flagInput, flagOutput, flagGPU string, flagNoGPU bool, flagQuality int) {
	info("1) 启动程序：FFmpeg 智能合并工具（命令行模式）")

	// Input dir
	absDir, err := filepath.Abs(flagInput)
	if err != nil {
		die("无效路径: %s", flagInput)
	}
	st, err := os.Stat(absDir)
	if err != nil || !st.IsDir() {
		die("目录不存在: %s", absDir)
	}
	inputDir = absDir

	// Quality
	qualityMode = flagQuality
	if qualityMode < 1 || qualityMode > 3 {
		die("质量模式必须为 1、2 或 3。")
	}

	// GPU
	if flagNoGPU {
		useGPU = false
		selectedEncoder = ""
	} else if flagGPU != "" {
		// User specified an encoder directly
		useGPU = true
		selectedEncoder = flagGPU
	} else if qualityMode != 1 {
		// Auto-detect: pick first available encoder
		detectHWEncoders()
		if len(hwEncoders) > 0 {
			useGPU = true
			selectedEncoder = hwEncoders[0]
			info("   自动选择编码器: %s", selectedEncoder)
		}
	}

	// Output
	cwd, _ := os.Getwd()
	if flagOutput == "" {
		defaultName := fmt.Sprintf("merged_%s.mp4", time.Now().Format("20060102_150405"))
		outputFile = filepath.Join(cwd, defaultName)
	} else {
		outputFile = flagOutput
		if filepath.Ext(outputFile) == "" {
			outputFile += ".mp4"
		}
	}
	outputFile, _ = filepath.Abs(outputFile)
	if err := os.MkdirAll(filepath.Dir(outputFile), 0o755); err != nil {
		die("无法创建输出目录: %v", err)
	}

	collectInputFiles(outputFile)

	info("3) 已按名称排序：")
	for i, file := range inputFiles {
		fmt.Printf("   %2d. %s\n", i+1, filepath.Base(file))
	}

	probeTotalDuration()
	showSystemInfo()

	if useGPU && strings.HasSuffix(selectedEncoder, "vaapi") {
		// Auto-select first VAAPI device in CLI mode
		matches, _ := filepath.Glob("/dev/dri/renderD*")
		if len(matches) > 0 {
			sort.Strings(matches)
			vaapiDevice = matches[0]
			info("   - VAAPI 设备: %s", vaapiDevice)
		} else {
			warn("未找到 VAAPI 设备，回退到软件编码。")
			useGPU = false
			selectedEncoder = ""
		}
	}

	ensureCopyModeCompatibility()
	finishMerge()
}

// ── Shared merge logic ──────────────────────────────────────────────────────

func finishMerge() {
	setEncodeArgs()

	info("   - 当前模式: %d（1=无损直合，2=轻压缩，3=重压缩）", qualityMode)
	if qualityMode == 1 {
		info("   - 编码设置: stream copy")
	} else if selectedEncoder != "" {
		info("   - 编码设置: %s", selectedEncoder)
	} else {
		info("   - 编码设置: 软件编码")
	}

	tmpDir, err := os.MkdirTemp("", "ffmpeg-merge-*")
	if err != nil {
		die("无法创建临时目录: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	concatFile := filepath.Join(tmpDir, "inputs.txt")
	metaFile := filepath.Join(tmpDir, "chapters.ffmeta")
	logFile := filepath.Join(tmpDir, "ffmpeg.log")

	if err := buildConcatList(concatFile); err != nil {
		die("无法写入 concat 文件: %v", err)
	}
	if err := buildMetadata(metaFile); err != nil {
		die("无法写入 metadata 文件: %v", err)
	}

	runMerge(concatFile, metaFile, logFile)
	printOutputSummary()
}

// ── Main ────────────────────────────────────────────────────────────────────

func main() {
	flagInput := flag.String("i", "", "输入视频文件目录")
	flagOutput := flag.String("o", "", "输出文件路径")
	flagQuality := flag.Int("q", 1, "质量模式: 1=快速无损, 2=轻度压缩, 3=重度压缩")
	flagGPU := flag.String("gpu", "", "指定 GPU 编码器 (如 h264_nvenc, hevc_vaapi)")
	flagNoGPU := flag.Bool("no-gpu", false, "禁用 GPU 加速，强制使用 CPU")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "FFmpeg 智能合并工具\n\n")
		fmt.Fprintf(os.Stderr, "用法:\n")
		fmt.Fprintf(os.Stderr, "  %s [选项]           有参数时命令行模式\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s                  无参数时交互模式\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\n选项:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\n示例:\n")
		fmt.Fprintf(os.Stderr, "  %s -i /path/to/videos -q 1 -o output.mp4\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s -i ./videos -q 2 -gpu hevc_nvenc\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s -i ./videos -q 3 --no-gpu\n", os.Args[0])
	}

	flag.Parse()

	requireTools()

	// If -i is provided, run in CLI mode; otherwise interactive
	if *flagInput != "" {
		runCLI(*flagInput, *flagOutput, *flagGPU, *flagNoGPU, *flagQuality)
	} else {
		runInteractive()
	}
}
