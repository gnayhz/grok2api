/** IP 脱敏:IPv4 保留前两段,IPv6 保留前两组;空值显示 "--"。 */
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
