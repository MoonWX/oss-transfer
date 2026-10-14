import type { Page } from "@playwright/test";

export const tasks = [
  {
    taskIndex: 1048,
    sourceS3Alias: "production",
    sourceBucket: "media-archive",
    destS3Alias: "backup",
    destBucket: "media-backup",
    status: "RUNNING",
    direction: "OSS_TO_OSS",
    listObjectCount: 186400,
    putObjectCount: 124200,
    putObjectSize: 396421234000,
    failedObjectCount: 0,
    skippedObjectCount: 1200,
    workerNode: "10.0.1.12",
    duration: "42m 18s",
    createTime: "2026-09-22 09:30:00",
  },
  {
    taskIndex: 1047,
    sourceS3Alias: "production",
    sourceBucket: "analytics-2026",
    destS3Alias: "backup",
    destBucket: "analytics-archive",
    status: "PENDING",
    direction: "OSS_TO_OSS",
    listObjectCount: 0,
    putObjectCount: 0,
    workerNode: "10.0.1.13",
    duration: "-",
    createTime: "2026-09-22 09:20:00",
  },
  {
    taskIndex: 1046,
    sourceS3Alias: "production",
    sourceBucket: "training-datasets",
    direction: "OSS_TO_LOCAL",
    sourceEndpoint: {
      type: "OSS",
      s3Alias: "production",
      bucket: "training-datasets",
    },
    destinationEndpoint: { type: "LOCAL", localPath: "/mnt/datasets" },
    localPath: "/mnt/datasets",
    status: "PARTIAL_FAILED",
    listObjectCount: 45200,
    putObjectCount: 45182,
    failedObjectCount: 18,
    putObjectSize: 192821234000,
    executionNodeIp: "10.0.1.12",
    duration: "1h 16m",
    createTime: "2026-09-22 08:15:00",
  },
  {
    taskIndex: 1045,
    sourceS3Alias: "production",
    sourceBucket: "application-logs",
    destS3Alias: "backup",
    destBucket: "logs-archive",
    status: "COMPLETED",
    listObjectCount: 243000,
    putObjectCount: 243000,
    failedObjectCount: 0,
    putObjectSize: 89721234000,
    workerNode: "10.0.1.13",
    duration: "28m 05s",
    createTime: "2026-09-21 23:00:00",
  },
];
export const storageConfigs = [
  {
    alias: "production",
    endpoint: "https://s3.example.test",
    accessKey: "TEST_ACCESS_KEY",
    secretKey: "TEST_SECRET_KEY",
  },
  {
    alias: "backup",
    endpoint: "https://backup.example.test",
    accessKey: "TEST_BACKUP_KEY",
    secretKey: "TEST_BACKUP_SECRET",
  },
];
const worker = {
  success: true,
  rateLimit: {
    list: { limitQps: 300, currentQps: 82 },
    put: { limitQps: 500, currentQps: 216 },
    head: { limitQps: 500, currentQps: 198 },
    bandwidth: { limitBps: 1000000000, currentBps: 648000000 },
  },
  taskStats: { pendingTasks: 1, runningTasks: 1, completedTasks: 8 },
  queueStatus: {
    listQueueSize: 24,
    listQueueCapacity: 10000,
    putQueueSize: 642,
    putQueueCapacity: 1000000,
    progressQueueSize: 12,
    progressQueueCapacity: 10000,
    failedKeyQueueSize: 0,
    failedKeyQueueCapacity: 10000,
    fallbackQueueSize: 0,
    fallbackQueueCapacity: 10000,
  },
};
export async function mockApi(
  page: Page,
  options: { authenticated?: boolean; empty?: boolean } = {},
) {
  if (options.authenticated !== false)
    await page.addInitScript(() => {
      if (sessionStorage.getItem("oss-transfer-test-seeded")) return;
      sessionStorage.setItem("oss-transfer-test-seeded", "true");
      sessionStorage.setItem("oss-transfer-token", "test-token");
      localStorage.setItem("oss-transfer-theme", "light");
    });
  const requests: { path: string; method: string; body: any }[] = [];
  await page.route("**/api/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;
    requests.push({
      path: path + url.search,
      method: request.method(),
      body: request.postDataJSON(),
    });
    let result: any = { success: true };
    if (path === "/api/setup/status")
      result = { success: true, configured: true };
    else if (path === "/api/auth/login")
      result = {
        success: true,
        token: "test-token",
        user: { username: "admin" },
      };
    else if (path === "/api/auth/current")
      result = { success: true, user: { username: "admin" } };
    else if (path === "/api/worker/node-info")
      result = {
        isMaster: true,
        localIp: "10.0.1.12",
        masterIp: "10.0.1.12",
        masterPort: 1685,
      };
    else if (path === "/api/master/status")
      result = {
        totalTasks: options.empty ? 0 : 4,
        pendingTasks: 1,
        runningTasks: 1,
        completedTasks: 1,
        partialFailedTasks: 1,
        totalPutObjectSize: 678963702000,
        totalPutObjectCount: 412382,
        totalFailedObjectCount: 18,
        tasks: options.empty ? [] : tasks,
      };
    else if (/^\/api\/master\/tasks\/\d+\/children(?:\/\d+)?$/.test(path)) {
      const parts = path.split("/");
      result = {
        success: true,
        data: {
          parentTaskId: Number(parts[4]),
          offset: parts[6]
            ? Number(parts[6]) - 1
            : Number(url.searchParams.get("offset") || 0),
          limit: parts[6] ? 1 : Number(url.searchParams.get("limit") || 25),
          items: [],
          totalCount: 0,
          partial: false,
        },
      };
    } else if (/^\/api\/master\/tasks\/\d+\/bandwidth-limit$/.test(path))
      result = {
        success: true,
        data: {
          limitMbps: null,
          applied: true,
          sharedTaskCount: 1,
          canEdit: true,
          nodes: [],
        },
      };
    else if (path === "/api/master/nodes" && request.method() === "GET")
      result = {
        success: true,
        data: {
          nodes: [
            { address: "10.0.1.12", label: "master", master: true },
            {
              address: "10.0.1.13",
              label: "agent",
              master: false,
              agent: true,
            },
          ],
          port: 1685,
          masterWebUrl: "http://10.0.1.12:1685",
        },
      };
    else if (path === "/api/master/worker-status")
      result = {
        success: true,
        workers: { "10.0.1.12": worker, "10.0.1.13": worker },
      };
    else if (path === "/api/master/s3-config" && request.method() === "GET")
      result = { success: true, configs: storageConfigs };
    else if (path.endsWith("/buckets"))
      result = {
        success: true,
        buckets: ["media-archive", "media-backup", "analytics-2026"],
      };
    else if (path === "/api/master/local-nodes")
      result = {
        success: true,
        data: {
          nodes: [
            {
              executionNodeIp: "10.0.1.12",
              reachable: true,
              localTransferSupported: true,
              advertisedLocalRoots: ["/mnt/datasets"],
              advertisedLocalBindings: [
                { storageId: "datasets", localPath: "/mnt/datasets" },
              ],
            },
            {
              executionNodeIp: "10.0.1.13",
              reachable: true,
              localTransferSupported: true,
              advertisedLocalRoots: ["/mnt/datasets"],
            },
          ],
        },
      };
    else if (path === "/api/master/local-transfers/status")
      result = {
        success: true,
        data: {
          tasks: tasks.filter((t) => t.direction === "OSS_TO_LOCAL"),
          shards: [],
          partial: false,
          unreachableWorkers: [],
        },
      };
    else if (path === "/api/master/local-transfers/failures")
      result = {
        success: true,
        data: {
          total: 1,
          items: [
            {
              taskId: 1046,
              taskIndex: 1046,
              key: "dataset/part-018.bin",
              sourceKey: "dataset/part-018.bin",
              errorMessage: "Connection reset",
              failureStage: "READ",
              executionNodeIp: "10.0.1.12",
            },
          ],
          partial: false,
        },
      };
    else if (path === "/api/master/local-transfers/validate")
      result = {
        success: true,
        data: {
          valid: true,
          nodeBindings: [
            {
              executionNodeIp: "10.0.1.12",
              localPath: "/mnt/data",
              valid: true,
            },
          ],
        },
      };
    else if (
      path === "/api/master/local-transfers" &&
      request.method() === "POST"
    )
      result = {
        success: true,
        data: { acceptedCount: 1, taskIds: [1049], parentTaskIds: [1049] },
      };
    else if (path === "/api/master/add-buckets")
      result = {
        success: true,
        addedCount: Array.isArray(request.postDataJSON())
          ? request.postDataJSON().length
          : 1,
        message: "任务已创建",
      };
    else if (path === "/api/master/import")
      result = { success: true, importedCount: 1 };
    else if (path === "/api/master/failed")
      result = {
        success: true,
        failedKeys: { "10.0.1.12": [], "10.0.1.13": [] },
      };
    else if (path === "/api/master/fallback")
      result = {
        success: true,
        fallbackObjects: { "10.0.1.12": [], "10.0.1.13": [] },
      };
    else if (path === "/api/master/deleted-tasks")
      result = { success: true, deletedTasks: [], totalCount: 0 };
    else if (path === "/api/master/rate-limit/history")
      result = { success: true, adjustments: [] };
    else if (path.includes("/local-transfers/"))
      result = {
        success: true,
        data: {
          accepted: 1,
          ignored: 0,
          failed: 0,
          partial: false,
          unreachableWorkers: [],
        },
      };
    await route.fulfill({ json: result });
  });
  return requests;
}
