# FFmpeg-Merge

[![Go](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

FFmpeg 智能视频合并工具 —— 自动章节标记、GPU 硬件加速、自然排序，支持交互式与命令行双模式。

> Smart video merging powered by FFmpeg — auto chapters, GPU acceleration, natural sorting, interactive & CLI modes.

## 功能

- 🎬 **智能合并**：自动收集目录内视频文件（mp4/mkv/mov/m4v/ts/webm），按自然排序合并
- 📑 **章节内嵌**：以文件名（去扩展名）作为章节标题，自动写入输出文件
- ⚡ **GPU 加速**：自动检测并验证可用硬件编码器（VAAPI / NVENC / QSV / VideoToolbox）
- 🎛️ **三种质量模式**：快速无损直合 / 轻度压缩 / 重度压缩
- 🖥️ **双模式运行**：无参数交互式问答，有参数命令行直接执行
- 📊 **实时进度条**：百分比、已用时间、预计剩余
- 🔒 **EBML 安全**：默认输出到当前工作目录，避免 ffmpeg concat 读取自身输出文件

## 依赖

- [FFmpeg](https://ffmpeg.org/)（含 ffprobe）
- [Go](https://go.dev/) 1.22+（仅编译时需要）

## 构建

```bash
go build -o ffmpeg-merge .
```

交叉编译：

```bash
GOOS=linux   GOARCH=amd64 go build -o ffmpeg-merge-linux-amd64 .
GOOS=darwin  GOARCH=arm64 go build -o ffmpeg-merge-darwin-arm64 .
GOOS=windows GOARCH=amd64 go build -o ffmpeg-merge-windows-amd64.exe .
```

## 使用

### 交互模式

```bash
./ffmpeg-merge
```

按提示依次输入目录、选择编码器、质量模式、输出路径。

<details>
<summary>交互模式流程示意</summary>

```
$ ./ffmpeg-merge

📂 请输入视频所在目录: /home/user/videos
   找到 5 个视频文件

⚡ 检测到可用 GPU 编码器:
   [1] hevc_vaapi
   [2] h264_vaapi
   [0] 不使用 GPU
   请选择: 1

🎛️ 选择质量模式:
   [1] 快速无损 (copy)
   [2] 轻度压缩
   [3] 重度压缩
   请选择: 2

📄 输出文件名: merged.mkv

🚀 开始合并...
   ████████████████████░░░░  78%  02:15 / ~00:38
✅ 完成！输出: /home/user/merged.mkv
```

</details>

### 命令行模式

```bash
./ffmpeg-merge -i /path/to/videos -q 1 -o output.mp4
```

| 参数 | 说明 |
|------|------|
| `-i <dir>` | 输入视频文件目录 |
| `-o <file>` | 输出文件路径 |
| `-q <1\|2\|3>` | 质量模式：1=快速无损，2=轻度压缩，3=重度压缩 |
| `-gpu <encoder>` | 指定 GPU 编码器（如 `hevc_vaapi`、`h264_nvenc`） |
| `--no-gpu` | 禁用 GPU，强制 CPU 编码 |

### 示例

```bash
# 无损快速合并
./ffmpeg-merge -i ./videos -q 1 -o merged.mp4

# VAAPI 轻度压缩
./ffmpeg-merge -i ./videos -q 2 -gpu hevc_vaapi -o merged.mkv

# CPU 重度压缩
./ffmpeg-merge -i ./videos -q 3 --no-gpu -o small.mp4
```

## 编码参数

| 模式 | VAAPI | NVENC | QSV | CPU |
|------|-------|-------|-----|-----|
| 1 快速无损 | `-c copy` | `-c copy` | `-c copy` | `-c copy` |
| 2 轻度压缩 | `-global_quality 23` | `-preset p5 -cq 23` | `-global_quality 23` | `-crf 20 medium` |
| 3 重度压缩 | `-global_quality 31` | `-preset p7 -cq 31` | `-global_quality 31` | `-crf 30 slow` |

## 许可

本项目基于 [MIT](LICENSE) 许可证开源。
