import { useMutation } from "@tanstack/react-query";
import { errorMessage } from "../api/client";
import { Button, Card, Notice, ProgressBar } from "../components/ui";
import { useDesktopState } from "./desktop";
import { useRecorder } from "./recorder";
import { checkForUpdate, dismissUpdate, installUpdate, useUpdateInSettings, useUpdater } from "./updater";
import type { UpdateInfo, UpdaterSnapshot } from "./updater";
import { checkFailedText, installFailedText, installView, noticeVisible, notesText, updateBlocker, updateHeadline, upToDateText } from "./updateView";

// 桌面版的自动更新（只在桌面版里用到）：页面顶上的新版本提示、登录前右下角的提示、设置里的「版本」。
// 检查、下载、安装都在 App 里（updater.ts），这里只显示和发命令；点了「更新并重启」才会装。

/** 现在用的版本：检查过的话用 App 报的，没检查过用启动时 App 给的。 */
function useCurrentVersion(updater: UpdaterSnapshot): string {
  const desktop = useDesktopState();
  return updater.current || desktop?.version || "";
}

/**
 * 页面上的新版本提示显示不显示（「以后再说」一天以后再提示，正在更新、出了错时一直显示）。
 * 新版本的消息每次推过来（App 每 6 小时检查一次）都会重新渲染、重新算一遍。
 */
function noticeShown(updater: UpdaterSnapshot): boolean {
  return noticeVisible({ available: updater.available, dismissed: updater.dismissed, phase: updater.phase, now: Date.now() });
}

/**
 * 页面顶上的新版本提示（登录以后，和「离线」「正在录音」这些提示条在一起，不挡页面上的按钮）。
 * 设置里的「版本」显示着的时候不显示，同样的内容不出现两遍。
 */
export function UpdateBanner() {
  const updater = useUpdater();
  const current = useCurrentVersion(updater);
  if (!noticeShown(updater) || updater.inSettings || !updater.available) return null;
  return (
    // 后面紧跟着录音的提示条时，隔开一点
    <section aria-label="新版本" className="flex flex-col gap-3 rounded-xl border border-line bg-white px-4 py-3.5 text-sm text-ink-2 [&+*]:mt-2.5">
      <UpdateDetails info={updater.available} updater={updater} current={current} onLater={dismissUpdate} />
    </section>
  );
}

/** 还没登录（填服务器地址、在浏览器里登录）时，新版本提示放在右下角：新版本可能正是登录不上的解决办法。 */
export function FloatingUpdateNotice() {
  const updater = useUpdater();
  const current = useCurrentVersion(updater);
  if (!noticeShown(updater) || !updater.available) return null;
  return (
    <section
      aria-label="新版本"
      className="fixed bottom-5 right-5 z-10 flex w-full max-w-sm flex-col gap-3 rounded-2xl border border-line bg-white p-4 text-sm text-ink-2 shadow-2xl"
    >
      <UpdateDetails info={updater.available} updater={updater} current={current} onLater={dismissUpdate} />
    </section>
  );
}

/**
 * 设置里的「版本」：现在用的版本、「检查更新」；有新版本时说是哪个版本、更新了什么，可以「更新并重启」
 * （页面顶上的提示点过「以后再说」也照样在这里显示）。
 */
export function UpdateSection() {
  useUpdateInSettings();
  const updater = useUpdater();
  const current = useCurrentVersion(updater);
  const check = useMutation({ mutationFn: checkForUpdate });
  const updating = updater.phase === "installing" || updater.phase === "installed";

  return (
    <Card className="flex flex-col gap-3 p-6">
      <h2 className="text-lg font-bold">版本</h2>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <span className="text-sm text-ink-2">zsmeet 桌面版 {current}</span>
        <Button size="sm" disabled={check.isPending || updating} onClick={() => check.mutate()}>
          {check.isPending ? "正在检查…" : "检查更新"}
        </Button>
      </div>
      {updater.available ? (
        <div className="flex flex-col gap-3 rounded-xl border border-line px-4 py-3.5 text-sm text-ink-2">
          {/* 上面已经写了现在的版本 */}
          <UpdateDetails info={updater.available} updater={updater} current="" />
        </div>
      ) : (
        check.isSuccess && <p className="text-sm text-ok">{upToDateText(check.data.current || current)}</p>
      )}
      {check.isError && <Notice tone="bad">{checkFailedText(errorMessage(check.error))}</Notice>}
      <p className="text-xs leading-relaxed text-muted">
        zsmeet 会自动检查新版本（打开后和之后每 6 小时一次），有新版本时页面顶上会提示；点了「更新并重启」才会下载、安装。新版本从 GitHub 下载，装之前核对签名，对不上的不装。
      </p>
    </Card>
  );
}

/**
 * 一个新版本：版本号、更新说明，「更新并重启」（正在录音、有录音没传完时变灰，旁边说为什么）、「以后再说」；
 * 更新时显示下载进度，没装上时说原因，可以再试。
 */
function UpdateDetails({ info, updater, current, onLater }: { info: UpdateInfo; updater: UpdaterSnapshot; current: string; onLater?: () => void }) {
  const { state: recorder } = useRecorder();
  const blocker = updateBlocker(recorder);
  const headline = updateHeadline(info, current);
  const notes = notesText(info.notes);
  const progress = installView(updater.phase, updater.progress);

  return (
    <>
      <div className="flex flex-col gap-0.5">
        <b className="font-semibold text-ink">{headline.title}</b>
        {headline.detail && <span className="text-xs text-muted">{headline.detail}</span>}
      </div>
      {/* 更新说明可能很长：最多显示几行，多的滚动着看 */}
      {notes && <p className="max-h-32 overflow-y-auto whitespace-pre-wrap text-[13px] leading-relaxed">{notes}</p>}
      {progress ? (
        // 不用 Notice：它是 role="status"，进度一直在变，读屏软件会一直念
        <div className="flex flex-col gap-2">
          <span className="leading-relaxed">{progress.text}</span>
          {progress.value !== null && <ProgressBar value={progress.value} />}
        </div>
      ) : (
        <>
          {updater.phase === "failed" && <Notice tone="bad">{installFailedText(updater.error)}</Notice>}
          <div className="flex flex-wrap items-center gap-2.5">
            <Button size="sm" variant="primary" disabled={!!blocker} title={blocker || undefined} onClick={() => void installUpdate()}>
              更新并重启
            </Button>
            {onLater && (
              <Button size="sm" variant="ghost" onClick={onLater}>
                以后再说
              </Button>
            )}
          </div>
          {blocker ? (
            <p className="text-xs leading-relaxed text-warn">{blocker}</p>
          ) : (
            // 没签名的 App 每个版本在系统看来都是新的（docs/desktop-guide.md 的「更新」也这么说）：先告诉用户会被问什么、点哪个
            <p className="text-xs leading-relaxed text-muted">
              更新要重启 zsmeet。安装包没有签名，macOS 会把新版本当成另一个 App：更新后第一次打开时可能会问能不能用钥匙串里的登录信息，点「始终允许」；第一次录音时可能会再问麦克风和录制系统音频的权限，点「允许」。
            </p>
          )}
        </>
      )}
    </>
  );
}
