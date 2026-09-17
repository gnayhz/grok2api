// 聊天消息展示(ui 层):消息项、助手内容渲染(XSS 安全化)、
// 工具活动、加载与错误态。状态与回调经命名 props 传入。
import { useMemo, type KeyboardEvent as ReactKeyboardEvent } from "react";
import { useTranslation } from "react-i18next";
import { marked } from "marked";
import { BrainCircuit, CheckCircle2, Globe, Loader2, Pencil, RefreshCw, Square, Trash2, TriangleAlert, Wrench } from "lucide-react";
import type { ChatMessage, ChatToolActivity } from "@/entities/creative-console/creative-console-api";
import { cn } from "@/shared/lib/cn";
import { Button } from "@/shared/ui/button";
import { Message, MessageContent, MessageFooter } from "@/shared/ui/message";
import { Spinner } from "@/shared/ui/spinner";
import { Textarea } from "@/shared/ui/textarea";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/shared/ui/tooltip";

export type ConversationMessage = ChatMessage & {
  id: string;
  reasoning?: string;
  tools?: ChatToolActivity[];
};


function boundedTableSpan(value: string | null): string {
  const parsed = Number.parseInt(value ?? "", 10);
  return Number.isInteger(parsed) && parsed >= 1 && parsed <= 100 ? String(parsed) : "";
}

function safeAssistantImage(value: string | null): string {
  const source = value?.trim() ?? "";
  if (!source) return "";
  if (source.startsWith("/v1/media/images/")) return source;
  try {
    const parsed = new URL(source);
    return parsed.protocol === "https:" ? parsed.toString() : "";
  } catch {
    return "";
  }
}

function safeAssistantLink(value: string | null): string {
  const link = value?.trim() ?? "";
  if (!link) return "";
  try {
    const parsed = new URL(link);
    return parsed.protocol === "http:" || parsed.protocol === "https:" || parsed.protocol === "mailto:" ? parsed.toString() : "";
  } catch {
    return "";
  }
}

function sanitizeAssistantHTML(content: string): string {
  if (typeof DOMParser === "undefined") return "";
  const source = content.trim();
  if (!/<\/?[a-z][^>]*>/i.test(source)) return "";
  const documentValue = new DOMParser().parseFromString(source, "text/html");
  const elements = Array.from(documentValue.body.querySelectorAll("*"));
  for (const element of elements) {
    if (!element.isConnected) continue;
    const tag = element.tagName.toLowerCase();
    if (discardedAssistantHTMLTags.has(tag)) {
      element.remove();
      continue;
    }
    if (!safeAssistantHTMLTags.has(tag)) {
      element.replaceWith(...Array.from(element.childNodes));
      continue;
    }
    const href = tag === "a" ? safeAssistantLink(element.getAttribute("href")) : "";
    const title = tag === "a" ? element.getAttribute("title")?.slice(0, 512) ?? "" : "";
    const imageSource = tag === "img" ? safeAssistantImage(element.getAttribute("src")) : "";
    const imageAlt = tag === "img" ? element.getAttribute("alt")?.slice(0, 512) ?? "" : "";
    const colSpan = tag === "td" || tag === "th" ? boundedTableSpan(element.getAttribute("colspan")) : "";
    const rowSpan = tag === "td" || tag === "th" ? boundedTableSpan(element.getAttribute("rowspan")) : "";
    const open = tag === "details" && element.hasAttribute("open");
    for (const attribute of Array.from(element.attributes)) element.removeAttribute(attribute.name);
    if (href) {
      element.setAttribute("href", href);
      element.setAttribute("target", "_blank");
      element.setAttribute("rel", "nofollow noopener noreferrer");
    }
    if (title) element.setAttribute("title", title);
    if (imageSource) {
      element.setAttribute("src", imageSource);
      element.setAttribute("alt", imageAlt);
      element.setAttribute("loading", "lazy");
      element.setAttribute("decoding", "async");
      element.setAttribute("referrerpolicy", "no-referrer");
    } else if (tag === "img") {
      element.remove();
      continue;
    }
    if (colSpan) element.setAttribute("colspan", colSpan);
    if (rowSpan) element.setAttribute("rowspan", rowSpan);
    if (open) element.setAttribute("open", "");
  }
  return documentValue.body.innerHTML;
}

export function RetryableError({ message, onRetry }: { message: string; onRetry: () => void }) {
  const { t } = useTranslation();
  return (
    <div role="alert" className="flex flex-col gap-2 rounded-md bg-destructive/8 px-3 py-2 text-xs leading-5 text-destructive sm:flex-row sm:items-center sm:justify-between">
      <span>{message}</span>
      <Button type="button" variant="ghost" size="sm" className="self-start text-destructive hover:text-destructive sm:self-auto" onClick={onRetry}>
        <RefreshCw />{t("common.retry")}
      </Button>
    </div>
  );
}

export function InlineError({ message }: { message: string }) {
  return <div role="alert" className="rounded-md bg-destructive/8 px-3 py-2 text-xs leading-5 text-destructive">{message}</div>;
}

export function LoadingResult({ text }: { text: string }) {
  return <div className="flex min-h-[20rem] items-center justify-center gap-3 text-xs text-muted-foreground"><Spinner className="size-5" />{text}</div>;
}

function ToolActivityItem({ tool }: { tool: ChatToolActivity }) {
  const { t } = useTranslation();
  const isWebSearch = tool.name === "web_search" || tool.type === "web_search_call";
  const isXSearch = tool.name === "x_search" || tool.type === "x_search_call";
  const label = isWebSearch
    ? t("creativeConsole.toolNames.webSearch")
    : isXSearch
      ? t("creativeConsole.toolNames.xSearch")
      : tool.name;
  const statusLabel = t(`creativeConsole.toolStatus.${tool.status}`);
  return (
    <div className="flex min-w-0 items-start gap-2 rounded-xl bg-secondary/45 px-3 py-2.5 text-xs">
      <span className="mt-0.5 text-muted-foreground">
        {isWebSearch ? <Globe className="size-3.5" /> : isXSearch ? <XSocialIcon className="size-3.5" /> : <Wrench className="size-3.5" />}
      </span>
      <div className="min-w-0 flex-1">
        <div className="flex min-w-0 items-center gap-2">
          <span className="truncate font-medium">{t("creativeConsole.toolCall")} · {label}</span>
          <span className="ml-auto flex shrink-0 items-center gap-1 text-muted-foreground">
            {tool.status === "in_progress" ? <Loader2 className="size-3 animate-spin" /> : tool.status === "failed" ? <TriangleAlert className="size-3 text-destructive" /> : <CheckCircle2 className="size-3" />}
            {statusLabel}
          </span>
        </div>
        {tool.detail ? <div className="mt-1 line-clamp-2 break-all leading-5 text-muted-foreground" title={tool.detail}>{tool.detail}</div> : null}
      </div>
    </div>
  );
}

function AssistantContent({ content }: { content: string }) {
  const renderedHTML = useMemo(() => renderAssistantMarkup(content), [content]);
  if (!renderedHTML) return <div className="w-full whitespace-pre-wrap break-words py-1 text-sm leading-6">{content}</div>;
  return (
    <div
      className="w-full break-words py-1 text-sm leading-6 [&>:first-child]:mt-0 [&>:last-child]:mb-0 [&_a]:text-primary [&_a]:underline [&_a]:underline-offset-2 [&_blockquote]:my-3 [&_blockquote]:border-l-2 [&_blockquote]:border-border [&_blockquote]:pl-3 [&_code]:rounded [&_code]:bg-secondary [&_code]:px-1 [&_code]:py-0.5 [&_h1]:mb-3 [&_h1]:mt-5 [&_h1]:text-xl [&_h1]:font-semibold [&_h2]:mb-2 [&_h2]:mt-4 [&_h2]:text-lg [&_h2]:font-semibold [&_h3]:mb-2 [&_h3]:mt-3 [&_h3]:font-semibold [&_hr]:my-4 [&_hr]:border-border [&_img]:my-3 [&_img]:max-h-[32rem] [&_img]:max-w-full [&_img]:rounded-xl [&_li]:my-1 [&_ol]:my-3 [&_ol]:list-decimal [&_ol]:pl-6 [&_p]:my-2 [&_pre]:my-3 [&_pre]:overflow-x-auto [&_pre]:rounded-xl [&_pre]:bg-secondary [&_pre]:p-3 [&_pre_code]:bg-transparent [&_pre_code]:p-0 [&_table]:my-3 [&_table]:w-full [&_table]:border-collapse [&_td]:border-b [&_td]:border-border [&_td]:px-3 [&_td]:py-2 [&_th]:border-b [&_th]:border-border [&_th]:px-3 [&_th]:py-2 [&_th]:text-left [&_ul]:my-3 [&_ul]:list-disc [&_ul]:pl-6"
      dangerouslySetInnerHTML={{ __html: renderedHTML }}
    />
  );
}

export function ChatMessageItem({
  message,
  loading = false,
  busy = false,
  editing = false,
  editDraft = "",
  onEditDraftChange,
  onStartEdit,
  onCancelEdit,
  onSaveEdit,
  onEditKeyDown,
  onRegenerate,
  onStop,
  onDelete,
}: {
  message: ConversationMessage;
  loading?: boolean;
  busy?: boolean;
  editing?: boolean;
  editDraft?: string;
  onEditDraftChange: (value: string) => void;
  onStartEdit: () => void;
  onCancelEdit: () => void;
  onSaveEdit: () => void;
  onEditKeyDown: (event: ReactKeyboardEvent<HTMLTextAreaElement>) => void;
  onRegenerate: () => void;
  onStop: () => void;
  onDelete: () => void;
}) {
  const { t } = useTranslation();
  const isUser = message.role === "user";
  const canStop = loading && !editing;
  const canRegenerate = !isUser && !editing && (!busy || loading);
  const canEdit = !loading && !busy;
  const canDelete = !loading && !busy && !editing;
  return (
    <Message align={isUser ? "end" : "start"}>
      <MessageContent className={cn(!isUser && "w-full max-w-full")}>
        {!isUser && message.reasoning ? (
          <div className="w-full rounded-xl bg-secondary/45 px-3 py-2.5 text-xs text-muted-foreground">
            <div className="mb-1.5 flex items-center gap-1.5 font-medium text-foreground/75"><BrainCircuit className="size-3.5" />{t("creativeConsole.thinkingProcess")}</div>
            <div className="whitespace-pre-wrap break-words leading-5">{message.reasoning}</div>
          </div>
        ) : null}
        {!isUser && message.tools?.length ? (
          <div className="flex w-full flex-col gap-1.5">
            {message.tools.map((tool: ChatToolActivity) => <ToolActivityItem key={tool.id} tool={tool} />)}
          </div>
        ) : null}
        {editing ? (
          <div className={cn("w-full space-y-2", isUser ? "max-w-full" : "")}>
            <Textarea
              value={editDraft}
              onChange={(event) => onEditDraftChange(event.target.value)}
              onKeyDown={onEditKeyDown}
              className="min-h-24 resize-y bg-background/70 text-sm"
              autoFocus
              aria-label={t("creativeConsole.editMessage")}
            />
            {!isUser ? (
              <p className="text-[11px] leading-4 text-muted-foreground">{t("creativeConsole.localEditNote")}</p>
            ) : null}
            <div className={cn("flex items-center gap-2", isUser && "justify-end")}>
              <Button type="button" variant="ghost" size="sm" onClick={onCancelEdit}>{t("creativeConsole.cancelEdit")}</Button>
              <Button type="button" size="sm" onClick={onSaveEdit} disabled={!editDraft.trim()}>
                {isUser ? t("creativeConsole.saveAndRegenerate") : t("creativeConsole.saveEdit")}
              </Button>
            </div>
          </div>
        ) : message.content || isUser ? (
          isUser ? (
            <div className="whitespace-pre-wrap break-words rounded-2xl rounded-br-md bg-secondary px-4 py-2.5 text-sm leading-6">{message.content}</div>
          ) : <AssistantContent content={message.content} />
        ) : null}
        {loading ? <div className="flex items-center gap-2 py-1 text-xs text-muted-foreground"><Spinner />{t("creativeConsole.streaming")}</div> : null}
        {!editing && (canStop || canRegenerate || canEdit || canDelete) ? (
          <MessageFooter className="gap-0.5 opacity-100 transition-opacity [@media(hover:hover)]:opacity-0 [@media(hover:hover)]:group-hover/message:opacity-100 [@media(hover:hover)]:group-focus-within/message:opacity-100">
            {canStop ? (
              <Tooltip>
                <TooltipTrigger asChild>
                  <Button type="button" variant="ghost" size="icon" className="size-7 rounded-full" aria-label={t("creativeConsole.stopGenerating")} onClick={onStop}>
                    <Square className="size-3.5 fill-current" />
                  </Button>
                </TooltipTrigger>
                <TooltipContent>{t("creativeConsole.stopGenerating")}</TooltipContent>
              </Tooltip>
            ) : null}
            {canRegenerate ? (
              <Tooltip>
                <TooltipTrigger asChild>
                  <Button type="button" variant="ghost" size="icon" className="size-7 rounded-full" aria-label={t("creativeConsole.regenerate")} onClick={onRegenerate}>
                    <RefreshCw className="size-3.5" />
                  </Button>
                </TooltipTrigger>
                <TooltipContent>{t("creativeConsole.regenerate")}</TooltipContent>
              </Tooltip>
            ) : null}
            {canEdit ? (
              <Tooltip>
                <TooltipTrigger asChild>
                  <Button type="button" variant="ghost" size="icon" className="size-7 rounded-full" aria-label={t("creativeConsole.editMessage")} onClick={onStartEdit}>
                    <Pencil className="size-3.5" />
                  </Button>
                </TooltipTrigger>
                <TooltipContent>{t("creativeConsole.editMessage")}</TooltipContent>
              </Tooltip>
            ) : null}
            {canDelete ? (
              <Tooltip>
                <TooltipTrigger asChild>
                  <Button type="button" variant="ghost" size="icon" className="size-7 rounded-full text-destructive hover:text-destructive" aria-label={t("creativeConsole.deleteMessage")} onClick={onDelete}>
                    <Trash2 className="size-3.5" />
                  </Button>
                </TooltipTrigger>
                <TooltipContent>{t("creativeConsole.deleteMessage")}</TooltipContent>
              </Tooltip>
            ) : null}
          </MessageFooter>
        ) : null}
      </MessageContent>
    </Message>
  );
}

export function XSocialIcon({ className }: { className?: string }) {
  return (
    <svg className={className} viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
      <path d="M18.244 2.25h3.308l-7.227 8.26 8.502 11.24H16.17l-5.214-6.817L4.99 21.75H1.68l7.73-8.835L1.254 2.25H8.08l4.713 6.231zm-1.161 17.52h1.833L7.084 4.126H5.117z" />
    </svg>
  );
}

const discardedAssistantHTMLTags = new Set([
  "applet", "audio", "base", "button", "canvas", "embed", "form", "frame", "frameset", "iframe", "input", "link",
  "math", "meta", "object", "picture", "script", "select", "source", "style", "svg", "template", "textarea", "video",
]);
function renderAssistantMarkup(content: string): string {
  const rendered = marked.parse(content, { async: false, breaks: true, gfm: true });
  return sanitizeAssistantHTML(typeof rendered === "string" ? rendered : "");
}

const safeAssistantHTMLTags = new Set([
  "a", "b", "blockquote", "br", "code", "del", "details", "div", "em", "h1", "h2", "h3", "h4", "h5", "h6",
  "hr", "i", "img", "kbd", "li", "mark", "ol", "p", "pre", "s", "span", "strong", "sub", "summary", "sup", "table",
  "tbody", "td", "tfoot", "th", "thead", "tr", "u", "ul",
]);