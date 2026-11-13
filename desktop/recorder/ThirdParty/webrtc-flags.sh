# 编译 WebRTC 音频处理和 abseil 用的参数，desktop/scripts/build-recorder.sh、desktop/recorder/test.sh
# 和 ThirdParty/prune.sh 都 source 这个文件，三处一定要一样：宏不一样，用到的头文件、编出来的代码就不一样。
# 这些参数照着 webrtc-audio-processing 2.1 的 meson.build 来（见 README.md），只是不用 meson：
# 只装了 Command Line Tools 也能编，不联网、不要 meson、ninja、cmake、pkg-config。
#
# 用法（bash 3.2 也能用）：
#   source desktop/recorder/ThirdParty/webrtc-flags.sh
#   webrtc_flags arm64        # 打印这个架构的公共参数，一行一个
#   webrtc_sources arm64      # 打印这个架构要编的源文件：「路径<TAB>这个文件额外的参数」，路径相对 ThirdParty/

# meson.build 里的 common_cflags（Linux、Windows 才用的去掉了）和 apm_flags
webrtc_common_defines=(
  -DNDEBUG
  -DWEBRTC_MAC
  -DWEBRTC_POSIX
  -DWEBRTC_LIBRARY_IMPL
  -DWEBRTC_ENABLE_SYMBOL_EXPORT
  -D_WINSOCKAPI_
  -DWEBRTC_APM_DEBUG_DUMP=0
)

# 这一份的目录（source 它的脚本在哪儿都行）
webrtc_third_party="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# 一个架构的公共参数：目标、优化、宏、头文件目录。不含 -isysroot、-std 和单个文件额外的参数
webrtc_flags() {
  local arch="$1"
  printf '%s\n' -target "$arch-apple-macos14.4" -O2 "${webrtc_common_defines[@]}"
  case "$arch" in
    # aarch64：meson 认为一定有 NEON（have_arm 只指 32 位 ARM）
    arm64) printf '%s\n' -DWEBRTC_ARCH_ARM64 -DWEBRTC_HAS_NEON ;;
    # x86_64：AVX2 的代码在单独的文件里（运行时按 CPU 选），SSE2 是 x86_64 本来就有的（meson 的 inline-sse 默认开）
    x86_64) printf '%s\n' -DWEBRTC_ENABLE_AVX2 ;;
    *) echo "不认识的架构：$arch" >&2; return 1 ;;
  esac
  printf '%s\n' -I "$webrtc_third_party/webrtc-audio-processing/webrtc" -I "$webrtc_third_party/abseil-cpp"
}

# 一个架构要编的源文件。webrtc-sources.txt 一行：<路径> [arm64|x86_64] [额外的参数…]，# 开头的是注释
webrtc_sources() {
  local arch="$1" line path rest word only extra
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line%%#*}"
    [[ -z "${line// /}" ]] && continue
    read -r path rest <<<"$line"
    only=""
    extra=""
    for word in $rest; do
      case "$word" in
        arm64 | x86_64) only="$word" ;;
        *) extra="${extra:+$extra }$word" ;;
      esac
    done
    if [[ -z "$only" || "$only" == "$arch" ]]; then
      printf '%s\t%s\n' "$path" "$extra"
    fi
  done <"$webrtc_third_party/webrtc-sources.txt"
}
