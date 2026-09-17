import { createObjectDecoder, hasShape, isString } from "@/shared/api/decoder";

// 管理端登录身份与会话凭据的传输形状。client.ts 只保留纯传输,
// 认证 DTO 与其解码器归位 shared/auth(登录/会话恢复/登出共用)。

export type AdminDTO = {
	id: string;
	username: string;
};

export type AuthTokensDTO = {
	accessToken: string;
	accessTokenExpiresAt: string;
	refreshTokenExpiresAt: string;
};

export type LoginResponseDTO = {
	admin: AdminDTO;
	tokens: AuthTokensDTO;
};

const adminValidator = hasShape({ id: isString, username: isString });
const authTokensValidator = hasShape({ accessToken: isString, accessTokenExpiresAt: isString, refreshTokenExpiresAt: isString });

export const decodeAdminDTO = createObjectDecoder<AdminDTO>("admin", { id: isString, username: isString });
export const decodeAuthTokensDTO = createObjectDecoder<AuthTokensDTO>("auth tokens", {
	accessToken: isString,
	accessTokenExpiresAt: isString,
	refreshTokenExpiresAt: isString,
});
export const decodeLoginResponseDTO = createObjectDecoder<LoginResponseDTO>("login", { admin: adminValidator, tokens: authTokensValidator });
export const decodeLoggedOut = createObjectDecoder<{ loggedOut: boolean }>("logout", { loggedOut: (value) => typeof value === "boolean" });
