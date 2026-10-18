# AI 会议总结：本地 Whisper + DeepSeek 配置

链路：房间音频 → Opus 解码 → 分片 → **whisper.cpp 本地转写** → 累积 → 定时调 **DeepSeek** 生成 markdown 纪要 → 信令推给前端。

## 一、编译 whisper.cpp 与拿模型

```bash
git clone https://github.com/ggml-org/whisper.cpp ~/codes/whisper/whisper.cpp
cd ~/codes/whisper/whisper.cpp
cmake -B build && cmake --build build -j8          # 产物在 build/bin/whisper-cli
bash models/download-ggml-model.sh small          # 模型 models/ggml-small.bin（约 465 MB）
```

`build/bin/whisper-cli` 会链接同目录下的 `libggml-*.dylib`，所以**不要把它单独拷走**。

## 二、`.env`（easylive-api）

```bash
STT_BACKEND=local
WHISPER_BIN=/Users/<你>/codes/whisper/whisper.cpp/build/bin/whisper-cli
WHISPER_MODEL_PATH=/Users/<你>/codes/whisper/whisper.cpp/models/ggml-small.bin
DEEPSEEK_API_KEY=sk-...
# 可选：DEEPSEEK_MODEL=deepseek-chat（默认）、WHISPER_LANG=zh（默认）、
#      AI_SUMMARY_INTERVAL=60（秒）、AI_CHUNK_SECONDS=30
```

### 两个真实踩过的坑（都会让 AI 总结"看起来没配"）

1. **`$PWD` 不会被展开** ✗。`godotenv` 是**字面量解析**，写
   `WHISPER_BIN=$PWD/../whisper.cpp/...` 得到的就是这串字符本身，不是路径。
   → **必须写绝对路径**（或相对 `.env` 所在目录也能工作的形式）。
2. **`.env` 里有一行没有 `=` 的非注释行，会让整个文件解析失败** ✗。
   症状是启动日志里出现 `未加载 .env … unexpected character`，然后**所有**变量都回默认值
   （包括 `DEEPSEEK_API_KEY`），但界面上只表现为"AI 总结未配置"，很容易误判成 Key 没填。
   → 改完 `.env` 一定看启动日志有没有 `未加载 .env`。

## 三、验证

```bash
# 1) 二进制 + 模型能否工作（用 whisper.cpp 自带的经典样本，应当输出 JFK 那句话）
WHISPER_CLI=~/codes/whisper/whisper.cpp/build/bin/whisper-cli
$WHISPER_CLI -m ~/codes/whisper/whisper.cpp/models/ggml-small.bin \
  -f ~/codes/whisper/whisper.cpp/samples/jfk.wav --output-txt --no-timestamps -l auto
cat ~/codes/whisper/whisper.cpp/samples/jfk.wav.txt

# 2) 后端启动日志里必须出现这一行
#    signaling: AI 总结已就绪（转写=local，DeepSeek=deepseek-chat）
#    若出现「AI 总结未配置（需要 …）」，就是上面两个坑之一

# 3) 会议里点「AI 总结」→ 探针
PROBE_URL=https://localhost:5174 PROBE_API=http://localhost:8080 node scripts/ai-probe.mjs
```

> 别用 `say -o xxx.aiff` 造测试音频：本机实测生成的文件只有 0.016 秒（近乎空），
> 转写必然失败，会误以为是 whisper 坏了。用 `samples/jfk.wav` 这类**已知good**的语音样本。

## 四、为什么默认走本地 whisper（而不是 OpenAI 云端）

- **零 Key、零调用成本**，音频**不出服务器**（会议内容敏感，这点在论文里值得单独说）；
- small 模型转写 30 秒音频约 5–10 秒（4 线程），相对 60 秒的纪要刷新周期完全够用；
- 云端模式仍可用：`STT_BACKEND=api` + `OPENAI_API_KEY`，切换只需改环境变量（`ai.Config` 的可插拔设计）。
