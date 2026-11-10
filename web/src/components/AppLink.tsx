import { Link, createPath, useLocation, useNavigate, useResolvedPath } from "react-router";
import type { LinkProps } from "react-router";

/**
 * 站内链接。站内跳转都用它，不直接用 react-router 的 Link（`make check` 会检查）；站外链接用普通的 <a>。
 *
 * 和 Link 只差一点：按住 Option（Windows 上是 Alt）点击时也照常跳转。Link 不处理带修饰键的点击，
 * 原样交给浏览器，而 Chrome、Safari 把 Option + 点击当成「下载这个链接」，
 * 下载下来的只是一个没有用的网页空壳（比如 settings.html）。
 * 按住 Cmd、Ctrl、Shift 点击（在新标签页、新窗口里打开）不受影响，还是交给浏览器。
 */
export function AppLink({ onClick, ...props }: LinkProps) {
  const navigate = useNavigate();
  const location = useLocation();
  const path = useResolvedPath(props.to, { relative: props.relative });

  return (
    <Link
      {...props}
      onClick={(e) => {
        onClick?.(e);
        const optionOnly = e.altKey && !e.metaKey && !e.ctrlKey && !e.shiftKey;
        const sameTab = !props.target || props.target === "_self";
        if (e.defaultPrevented || e.button !== 0 || !optionOnly || !sameTab || props.reloadDocument) return;
        // 拦下浏览器的下载，再像普通点击一样跳转。点的就是当前页面时和 Link 一样替换历史记录，
        // 免得「后退」要多按一次
        e.preventDefault();
        navigate(props.to, {
          replace: props.replace ?? createPath(location) === createPath(path),
          state: props.state,
          relative: props.relative,
        });
      }}
    />
  );
}
