import { useMutation, useQuery } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import type { FormEvent } from "react";
import { api, errorMessage, queryClient, unwrap } from "../api/client";
import { Button, Card, Field, Input, Notice, Select, Spinner } from "../components/ui";
import { isEn } from "../i18n/lang";
import { tr, useLang } from "../i18n/react";
import { dateTime } from "../lib/format";
import { Check } from "./FeishuSettings";

const DEFAULT_ASR_URL = "https://api.deepseek.com/v1/tools/asr";

/**
 * 语音识别配置：默认只用本机 SenseVoice（录音不出服务器）；
 * 管理员可选开启 DeepSeek 在线识别，由本机提供字级时间轴与兜底。
 */
export function ASRSettingsSection() {
  useLang();
  const saved = useQuery({
    queryKey: ["settings", "asr"],
    queryFn: () => unwrap(api.GET("/api/settings/asr")),
  });

  const [provider, setProvider] = useState<"local" | "deepseek">("local");
  const [baseURL, setBaseURL] = useState(DEFAULT_ASR_URL);
  const [apiKey, setAPIKey] = useState("");
  const [clearKey, setClearKey] = useState(false);
  const [loaded, setLoaded] = useState(false);

  useEffect(() => {
    if (saved.data && !loaded) {
      setProvider(saved.data.provider);
      setBaseURL(saved.data.base_url || DEFAULT_ASR_URL);
      setLoaded(true);
    }
  }, [saved.data, loaded]);

  const test = useMutation({
    mutationFn: () =>
      unwrap(
        api.POST("/api/settings/asr/test", {
          body: { base_url: baseURL || undefined, api_key: apiKey || undefined },
        }),
      ),
  });

  const save = useMutation({
    mutationFn: () =>
      unwrap(
        api.PUT("/api/settings/asr", {
          body: {
            provider,
            base_url: baseURL || undefined,
            api_key: provider === "deepseek" ? apiKey || undefined : undefined,
            clear_api_key: provider === "local" && clearKey ? true : undefined,
          },
        }),
      ),
    onSuccess: (data) => {
      queryClient.setQueryData(["settings", "asr"], data);
      setAPIKey("");
      setClearKey(false);
    },
  });

  const edited = () => {
    test.reset();
    save.reset();
  };

  const submit = (e: FormEvent) => {
    e.preventDefault();
    test.reset();
    save.mutate();
  };

  if (saved.isPending) return <Spinner label={tr("正在加载配置…", "Loading settings…")} />;
  if (saved.isError) return <Notice tone="bad">{errorMessage(saved.error)}</Notice>;

  const pending = test.isPending || save.isPending;
  const canTest = Boolean(apiKey.trim() || saved.data.api_key_set);

  return (
    <Card className="p-6">
      <form onSubmit={submit} className="flex flex-col gap-4">
        <div className="flex flex-col gap-1">
          <h2 className="text-lg font-bold">{tr("语音识别", "Speech Recognition")}</h2>
          <p className="text-[13px] leading-relaxed text-muted">
            {tr(
              "把会议录音转成文字。默认在服务器本机识别，录音不出服务器；也可以打开 DeepSeek 在线识别来减少同音错字和口语专名误识。全团队共用这一份配置，对之后新转写的会议生效。",
              "Transcribes meeting audio into text. Runs locally on the server by default so audio never leaves your infrastructure; optionally enable DeepSeek online ASR for higher accuracy on homophones and proper nouns. Shared across the team and applies to newly transcribed meetings.",
            )}
          </p>
        </div>

        {saved.data.last_error && (
          <Notice tone="warn">
            {isEn()
              ? `Last online ASR failure: ${saved.data.last_error}${saved.data.last_error_at ? ` (${dateTime(saved.data.last_error_at)})` : ""}. Affected meetings automatically fell back to local ASR; click "Save" after fixing credentials to clear this notice.`
              : `最近一次在线识别失败：${saved.data.last_error}${saved.data.last_error_at ? `（${dateTime(saved.data.last_error_at)}）` : ""}。受影响的会议已自动改用本机识别；处理好凭证后点「保存」就会清掉这条提示。`}
          </Notice>
        )}

        <Field label={tr("识别方式", "Recognition mode")} htmlFor="asr-provider">
          <Select
            id="asr-provider"
            className="max-w-md"
            value={provider}
            onChange={(e) => {
              setProvider(e.target.value === "deepseek" ? "deepseek" : "local");
              setClearKey(false);
              edited();
            }}
          >
            <option value="local">{tr("本机识别（默认，录音不出服务器）", "Local recognition (default — audio stays on server)")}</option>
            <option value="deepseek">{tr("DeepSeek 在线识别（准确率更高，录音分段发给 DeepSeek）", "DeepSeek online ASR (higher accuracy — audio chunks sent to DeepSeek)")}</option>
          </Select>
        </Field>

        {provider === "deepseek" ? (
          <>
            <ul className="flex list-disc flex-col gap-1.5 rounded-xl bg-canvas py-3 pl-8 pr-4 text-[13px] leading-relaxed text-ink-2">
              <li>
                <span className="font-semibold text-ink">{tr("录音外发与计费：", "External audio & billing: ")}</span>
                {tr(
                  "开启后，新转写（及手动点「重新转写」）的会议会把有人说话的录音切成不超过 100 秒的小段，发给 DeepSeek（深度求索）服务器识别，按音频时长计费，受 DeepSeek 的服务条款与数据政策约束。涉密会议请保持「本机识别」。",
                  "When enabled, newly transcribed (or re-transcribed) meetings split speech segments (up to 100s each) and send them to DeepSeek for recognition, billed by audio duration under DeepSeek's terms and privacy policy. Keep \"Local recognition\" for confidential meetings.",
                )}
              </li>
              <li>
                <span className="font-semibold text-ink">{tr("时间轴与自动兜底：", "Timestamps & automatic fallback: ")}</span>
                {tr(
                  "本机 SenseVoice 仍会运行，用来提供逐字时间点、区分说话人；遇到网络异常、额度用完或某一段对齐不上时，会自动退回用本机识别的结果，不会让整场会议失败。",
                  "Local SenseVoice still runs to provide word-level timestamps and speaker diarization; if network errors, quota exhaustion, or alignment issues occur, affected segments automatically fall back to local results so the meeting never fails.",
                )}
              </li>
            </ul>

            <Field
              label={tr("接口地址", "Endpoint URL")}
              htmlFor="asr-url"
              hint={isEn() ? `Defaults to ${DEFAULT_ASR_URL}; usually no change needed` : `默认是 ${DEFAULT_ASR_URL}，一般不用改`}
            >
              <Input
                id="asr-url"
                type="url"
                required
                maxLength={500}
                placeholder={DEFAULT_ASR_URL}
                value={baseURL}
                onChange={(e) => {
                  setBaseURL(e.target.value);
                  edited();
                }}
              />
            </Field>

            <Field
              label="API Key"
              htmlFor="asr-key"
              hint={
                saved.data.api_key_set
                  ? tr("已加密保存。留空表示沿用已保存的 Key", "Encrypted and saved. Leave blank to keep the existing key")
                  : tr("在 DeepSeek 开放平台申请的 API Key", "API key from the DeepSeek platform")
              }
            >
              <Input
                id="asr-key"
                type="password"
                autoComplete="off"
                required={!saved.data.api_key_set}
                maxLength={500}
                placeholder={saved.data.api_key_set ? "••••••••" : "sk-..."}
                value={apiKey}
                onChange={(e) => {
                  setAPIKey(e.target.value);
                  edited();
                }}
              />
            </Field>
          </>
        ) : (
          <>
            <p className="rounded-xl bg-canvas px-4 py-3 text-[13px] leading-relaxed text-ink-2">
              {tr(
                "录音只在你自己的服务器上由 SenseVoice 模型转成文字，不会发给任何外部服务。",
                "Audio is transcribed locally on your server by SenseVoice and never sent to external services.",
              )}
            </p>
            {saved.data.api_key_set && (
              <Check
                id="asr-clear-key"
                label={tr("同时清除已保存的 DeepSeek API Key", "Also clear saved DeepSeek API Key")}
                hint={tr("不勾选则保留已保存的 Key，以后再打开在线识别时不用重新填。", "Leave unchecked to keep the saved key for future use.")}
                checked={clearKey}
                disabled={pending}
                onChange={(on) => {
                  setClearKey(on);
                  edited();
                }}
              />
            )}
          </>
        )}

        {test.isSuccess && <Notice tone="ok">{tr("连接正常，API Key 可用。记得点「保存」。", "Connection succeeded and API key is valid. Remember to click \"Save\".")}</Notice>}
        {test.isError && <Notice tone="bad">{errorMessage(test.error)}</Notice>}
        {save.isSuccess && (
          <Notice tone="ok">
            {provider === "deepseek"
              ? tr("已保存。之后新转写的会议会使用 DeepSeek 在线识别。", "Saved. Newly transcribed meetings will use DeepSeek online ASR.")
              : tr("已保存。之后新转写的会议只用服务器本机识别，录音不外发。", "Saved. Newly transcribed meetings will use local recognition only.")}
          </Notice>
        )}
        {save.isError && <Notice tone="bad">{errorMessage(save.error)}</Notice>}

        <div className="flex flex-wrap gap-2.5">
          {provider === "deepseek" && (
            <Button
              disabled={pending || !canTest}
              onClick={() => {
                save.reset();
                test.mutate();
              }}
            >
              {test.isPending ? tr("正在测试…", "Testing…") : tr("测试连接", "Test connection")}
            </Button>
          )}
          <Button type="submit" variant="primary" disabled={pending}>
            {save.isPending ? tr("正在保存…", "Saving…") : tr("保存", "Save")}
          </Button>
        </div>
      </form>
    </Card>
  );
}
