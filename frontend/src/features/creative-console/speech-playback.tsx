import { ExternalLink } from "lucide-react";
import { useEffect, useRef } from "react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import type { TTSResult } from "./creative-console-api";

export function SpeechPlayback({ result }: { result: TTSResult }) {
  const { t } = useTranslation();
  const audioRef = useRef<HTMLAudioElement>(null);
  const downloadRef = useRef<HTMLAnchorElement>(null);

  useEffect(() => {
    const audio = audioRef.current;
    const download = downloadRef.current;
    if (!audio || !download) return;
    // Only a committed display owns a native URL. Decoding a late response or
    // abandoning a render must not allocate a resource without a cleanup owner.
    const owned = typeof result.source !== "string";
    const url = typeof result.source === "string" ? result.source : URL.createObjectURL(result.source);
    audio.src = url;
    download.href = url;
    return () => {
      audio.pause();
      audio.removeAttribute("src");
      audio.load();
      download.removeAttribute("href");
      if (owned) URL.revokeObjectURL(url);
    };
  }, [result.source]);

  return (
    <div className="mx-auto flex w-full max-w-3xl flex-col gap-3 rounded-2xl bg-secondary/40 p-4">
      <audio ref={audioRef} controls className="w-full" />
      <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
        <span>{result.contentType}{typeof result.duration === "number" ? ` · ${result.duration.toFixed(2)}s` : ""}</span>
        <Button variant="secondary" size="sm" asChild><a ref={downloadRef} download="speech.mp3"><ExternalLink />{t("creativeConsole.open")}</a></Button>
      </div>
    </div>
  );
}
