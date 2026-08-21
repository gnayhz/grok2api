import { Component, type ErrorInfo, type ReactNode } from "react";
import { useTranslation } from "react-i18next";

interface BoundaryProps {
  children: ReactNode;
}

interface BoundaryState {
  error: Error | null;
}

/**
 * AppErrorBoundary：捕获渲染期异常，避免整树卸载白屏——单个页面的
 * 数据异常只降级该层（错误卡片 + 重载入口），管理台其余功能不受影响。
 * 事件回调（onClick 等）中的异常不经过此处，由既有 Promise/全局处理。
 */
class AppErrorBoundary extends Component<BoundaryProps, BoundaryState> {
  state: BoundaryState = { error: null };

  static getDerivedStateFromError(error: Error): BoundaryState {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo): void {
    console.error("ui_render_error", error.message, info.componentStack);
  }

  render(): ReactNode {
    if (this.state.error !== null) {
      return <ErrorCard error={this.state.error} onReload={() => window.location.reload()} />;
    }
    return this.props.children;
  }
}

function ErrorCard({ error, onReload }: { error: Error; onReload: () => void }) {
  const { t } = useTranslation();
  return (
    <div className="flex min-h-screen items-center justify-center p-8">
      <div className="max-w-md space-y-4 rounded-lg border p-6 text-center">
        <h1 className="text-lg font-semibold">{t("errorBoundaryTitle")}</h1>
        <p className="text-sm text-muted-foreground">{t("errorBoundaryDescription")}</p>
        <p className="break-all font-mono text-xs text-muted-foreground">{error.message}</p>
        <button
          type="button"
          onClick={onReload}
          className="rounded-md bg-primary px-4 py-2 text-sm text-primary-foreground hover:bg-primary/90"
        >
          {t("errorBoundaryReload")}
        </button>
      </div>
    </div>
  );
}

export { AppErrorBoundary };
