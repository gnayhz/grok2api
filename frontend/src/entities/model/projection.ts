import type { ModelRouteDTO } from "@/entities/model/types";

// 模型路由投影:面向 UI 的纯派生(同一 publicId 只保留第一个路由),
// 供 features 层(创意控制台/API 文档)共用,避免各自复制实现。

/** 按 publicId 去重,保留首次出现的路由。 */
export function uniqueModelsByPublicID(models: ModelRouteDTO[]): ModelRouteDTO[] {
	const seen = new Set<string>();
	return models.filter((model) => {
		if (seen.has(model.publicId)) return false;
		seen.add(model.publicId);
		return true;
	});
}
