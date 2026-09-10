package account

// MatchWebBuildConversion decides whether a successful conversion can establish
// its explicit relation. Both references identify actual material; zero is a
// valid legacy generation and does not bypass matching.
func MatchWebBuildConversion(web, build, currentWeb, currentBuild CredentialRef) bool {
	return web.AccountID != 0 && build.AccountID != 0 && web.AccountID != build.AccountID &&
		web.Provider == ProviderWeb && build.Provider == ProviderBuild &&
		web == currentWeb && build == currentBuild
}
