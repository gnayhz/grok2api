// media 文案归 feature 并由 app 注册;键名保持不变。
export const mediaZh = {

        images: {
          title: "图库",
          description: "浏览已生成的图片资源",
          search: "搜索 ID、类型或哈希",
          empty: "暂无图片",
          noMatches: "未找到匹配的图片",
          totalImages: "图片总数",
          totalBytes: "占用空间",
          pageSummary: "显示 {{count}} / {{total}} 张",
          openImage: "打开图片 {{id}}",
          deleteTitle: "删除所选 {{count}} 张图片？",
          deleteDescription: "图片文件与图库记录将被永久删除，现有图片链接也会失效。此操作无法撤销。",
          deleted: "已删除 {{count}} 张图片",
        },
        videos: {
          title: "视频库",
          description: "查看视频生成任务记录",
          search: "搜索提示词或 ID",
          empty: "暂无视频任务",
          totalJobs: "任务总数",
          queued: "排队中",
          inProgress: "进行中",
          completed: "已完成",
          failed: "失败",
          prompt: "提示词",
          model: "模型",
          status: "状态",
          statusProgress: "状态 / 进度",
          spec: "规格",
          owner: "归属",
          time: "时间",
          createdShort: "创建",
          completedShort: "完成",
          preview: "预览视频",
          previewTitle: "视频预览",
          previewUnavailable: "本地视频不可用",
          deleteTitle: "删除 {{count}} 条视频记录？",
          deleteDescription: "任务记录及其本地视频将被永久删除，现有视频链接会失效。此操作无法撤销。",
          deleted: "已删除 {{count}} 条视频记录",
          seconds: "{{count}} 秒",
          pageSummary: "显示 {{count}} / {{total}} 条",
        },
        videoStatus: {
          queued: "排队中",
          in_progress: "进行中",
          completed: "已完成",
          failed: "失败",
        }
};

export const mediaEn = {
 images: { title: "Gallery", description: "Browse generated image assets", search: "Search ID, type, or hash", empty: "No images", noMatches: "No matching images", totalImages: "Total images", totalBytes: "Storage", pageSummary: "Showing {{count}} / {{total}}", openImage: "Open image {{id}}", deleteTitle: "Delete {{count}} selected images?", deleteDescription: "The image files and gallery records will be permanently removed, and existing image links will stop working. This cannot be undone.", deleted: "Deleted {{count}} images" }, videos: { title: "Video Gallery", description: "View video generation job records", search: "Search prompt or ID", empty: "No video jobs", totalJobs: "Total jobs", queued: "Queued", inProgress: "In progress", completed: "Completed", failed: "Failed", prompt: "Prompt", model: "Model", status: "Status", statusProgress: "Status / progress", spec: "Spec", owner: "Owner", time: "Time", createdShort: "Created", completedShort: "Done", preview: "Preview video", previewTitle: "Video preview", previewUnavailable: "Local video unavailable", deleteTitle: "Delete {{count}} video records?", deleteDescription: "The task records and their local videos will be permanently removed, and existing video links will stop working. This cannot be undone.", deleted: "Deleted {{count}} video records", seconds: "{{count}} sec", pageSummary: "Showing {{count}} / {{total}}" }, videoStatus: { queued: "Queued", in_progress: "In progress", completed: "Completed", failed: "Failed" }
};
