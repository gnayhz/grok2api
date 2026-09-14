/**
 * Proxy Network formatting and presentation utilities.
 */

export function formatTimeAgo(dateString: string | null | undefined, locale?: string): string {
	if (!dateString) return "--";
	const timestamp = new Date(dateString).getTime();
	if (Number.isNaN(timestamp)) return "--";

	const elapsedSeconds = Math.floor((Date.now() - timestamp) / 1000);
	const isEn = Boolean(locale && locale.toLowerCase().startsWith("en"));

	if (elapsedSeconds < 10) {
		return isEn ? "just now" : "刚刚";
	}
	if (elapsedSeconds < 60) {
		return isEn ? `${elapsedSeconds}s ago` : `${elapsedSeconds} 秒前`;
	}

	const minutes = Math.floor(elapsedSeconds / 60);
	if (minutes < 60) {
		return isEn ? `${minutes}m ago` : `${minutes} 分钟前`;
	}

	const hours = Math.floor(minutes / 60);
	if (hours < 24) {
		return isEn ? `${hours}h ago` : `${hours} 小时前`;
	}

	const days = Math.floor(hours / 24);
	return isEn ? `${days}d ago` : `${days} 天前`;
}

export function maskIP(ip: string | undefined): string {
	if (!ip) return "--";
	// IPv4 mask: 1.2.3.4 -> 1.2.*.*
	if (ip.includes(".")) {
		const parts = ip.split(".");
		if (parts.length === 4) {
			return `${parts[0]}.${parts[1]}.*.*`;
		}
	}
	// IPv6 mask: 2400:cb00:2048:1::c629:d7a2 -> 2400:cb00:****:****
	if (ip.includes(":")) {
		const parts = ip.split(":");
		if (parts.length >= 4) {
			return `${parts[0]}:${parts[1]}:****:****`;
		}
	}
	return ip;
}

export function getLatencyTone(ms: number | null | undefined): {
	textClass: string;
	badgeClass: string;
	label: string;
} {
	if (ms === null || ms === undefined || ms <= 0) {
		return {
			textClass: "text-muted-foreground",
			badgeClass: "border-muted-foreground/30 text-muted-foreground bg-muted/20",
			label: "未知",
		};
	}
	if (ms < 250) {
		return {
			textClass: "text-emerald-500 font-bold",
			badgeClass: "border-emerald-500/30 text-emerald-600 dark:text-emerald-400 bg-emerald-500/10",
			label: "极速",
		};
	}
	if (ms < 650) {
		return {
			textClass: "text-blue-500 font-bold",
			badgeClass: "border-blue-500/30 text-blue-600 dark:text-blue-400 bg-blue-500/10",
			label: "良好",
		};
	}
	if (ms < 1200) {
		return {
			textClass: "text-amber-500 font-bold",
			badgeClass: "border-amber-500/30 text-amber-600 dark:text-amber-400 bg-amber-500/10",
			label: "稍缓",
		};
	}
	return {
		textClass: "text-rose-500 font-bold",
		badgeClass: "border-rose-500/30 text-rose-600 dark:text-rose-400 bg-rose-500/10",
		label: "迟滞",
	};
}
