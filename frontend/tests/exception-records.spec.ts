import { expect, test } from "@playwright/test";
import {
  exceptionCsv,
  exceptionDetails,
  missingExceptionNodes,
} from "../src/lib/exceptionRecords";
import { translate } from "../src/lib/i18n";

const nodes = ["192.0.2.10", "192.0.2.11"];
// Shape returned by the real missing-object task used in cloud acceptance.
const failure = {
  taskIndex: 27,
  parentTaskId: 27,
  childTaskId: "27-1",
  workerNode: nodes[0],
  executionNodeIp: nodes[0],
  direction: "OSS_TO_OSS",
  sourceS3Alias: "fixture-storage",
  sourceBucket: "source",
  destS3Alias: "fixture-storage",
  destBucket: "destination",
  key: "missing-object.txt",
  logicalKey: "missing-object.txt",
  destinationKey: "missing-object.txt",
  objectSize: 0,
  stage: "TRANSFER",
  errorCode: "TRANSFER_FAILED",
  reasonCode: "TRANSFER_FAILED",
  errorMessage: "HeadObject: StatusCode: 404",
  retryCount: 0,
  observedAt: 1792130530972,
  timestamp: 1792130530972,
  timestampFormatted: "2026-10-16T06:02:10Z",
  targetCommitted: false,
};

test("a task queries only its assigned agent, even when live membership changes", () => {
  const snapshot = {
    queriedWorkers: [nodes[0]],
    respondingNodes: [nodes[0]],
    failedWorkers: [],
  };
  expect(missingExceptionNodes(snapshot, nodes)).toEqual([]);
  expect(missingExceptionNodes(snapshot, [...nodes, "192.0.2.99"])).toEqual([]);
  expect(
    missingExceptionNodes({ queriedWorkers: [], respondingNodes: [] }, nodes),
  ).toEqual([]);
});

test("missing participants and explicit failures remain visible, including cached rows", () => {
  expect(
    missingExceptionNodes(
      { queriedWorkers: nodes, respondingNodes: [nodes[0]] },
      [],
    ),
  ).toEqual([nodes[1]]);
  expect(
    missingExceptionNodes(
      {
        queriedWorkers: nodes,
        respondingNodes: nodes,
        failedWorkers: [nodes[1]],
        unreachableWorkers: [nodes[1], "192.0.2.99"],
      },
      [],
    ),
  ).toEqual([nodes[1], "192.0.2.99"]);
  expect(missingExceptionNodes({ respondingNodes: [nodes[0]] }, nodes)).toEqual(
    [nodes[1]],
  );
});

test("details use the public child ID and preserve zero/false without duplicate aliases", () => {
  const details = exceptionDetails(failure);
  const values = Object.fromEntries(
    details.map(({ key, value }) => [key, value]),
  );
  expect(values).toMatchObject({
    parentTaskId: 27,
    childTaskId: "27-1",
    workerNode: nodes[0],
    objectSize: 0,
    retryCount: 0,
    targetCommitted: false,
    errorMessage: "HeadObject: StatusCode: 404",
  });
  for (const key of [
    "taskIndex",
    "executionNodeIp",
    "logicalKey",
    "reasonCode",
    "timestamp",
  ])
    expect(values).not.toHaveProperty(key);
  // RFC3339 seconds and millisecond timestamps describe the same recorded event.
  expect(values).not.toHaveProperty("observedAt");
  expect(exceptionDetails({ taskIndex: 99 })).toEqual([
    { key: "parentTaskId", label: "任务 ID", value: 99 },
  ]);
});

test("different diagnostic values are retained", () => {
  const values = Object.fromEntries(
    exceptionDetails({
      ...failure,
      reasonCode: "SOURCE_MISSING",
      logicalKey: "logical/alias",
      observedAt: 1792130532972,
    }).map(({ key, value }) => [key, value]),
  );
  expect(values.reasonCode).toBe("SOURCE_MISSING");
  expect(values.logicalKey).toBe("logical/alias");
  expect(values.observedAt).toBe("2026-10-16T06:02:12.972Z");
});

test("CSV exports parent/child IDs and protects spreadsheet cells", () => {
  const csv = exceptionCsv([
    { ...failure, key: "=SUM(A1)", errorMessage: 'bad "key"\n404' },
  ]);
  const header = csv.split("\r\n")[0];
  expect(header).toMatch(/^\uFEFF"parentTaskId","childTaskId","workerNode"/);
  expect(header).not.toMatch(/taskIndex|storageId|executionNodeIp/);
  expect(csv).toContain('\r\n"27","27-1","192.0.2.10","OSS_TO_OSS"');
  expect(csv).toContain('"\'=SUM(A1)"');
  expect(csv).toContain('"bad ""key""\n404"');
});

test("all displayed diagnostic labels have English and Japanese translations", () => {
  for (const field of exceptionDetails({
    ...failure,
    reasonCode: "SOURCE_MISSING",
    logicalKey: "other",
  })) {
    expect(translate("en-US", field.label)).not.toBe(field.label);
    expect(translate("ja-JP", field.label)).not.toBe(field.label);
  }
  expect(translate("en-US", "目标已写入")).toBe("Destination written");
  expect(translate("ja-JP", "否")).toBe("いいえ");
});

test("retry exports retain the original failure and the latest per-object outcome", () => {
  const row = {
    ...failure,
    retryStatus: "FAILED",
    retryCount: 2,
    retryUpdatedAt: 1792130590972,
    retryError: "UploadPart: permission denied",
    retryExecutionNodeIp: "agent-replacement",
    retryExecutionNodeDisplay: {
      displayName: "backup",
      displayAddress: "192.0.2.11",
    },
  };
  const values = Object.fromEntries(
    exceptionDetails(row).map(({ key, value }) => [key, value]),
  );
  expect(values).toMatchObject({
    errorMessage: failure.errorMessage,
    retryStatus: "FAILED",
    retryCount: 2,
    retryExecutionNodeIp: "backup (192.0.2.11)",
    retryUpdatedAt: "2026-10-16T06:03:10.972Z",
    retryError: "UploadPart: permission denied",
  });
  const csv = exceptionCsv([row]);
  expect(csv).toContain(
    '"retryStatus","retryCount","retryExecutionNodeIp","retryUpdatedAt","retryError"',
  );
  expect(csv).toContain(
    '"FAILED","2","backup (192.0.2.11)","2026-10-16T06:03:10.972Z","UploadPart: permission denied"',
  );
  expect(csv).toContain(failure.errorMessage);
  for (const field of exceptionDetails(row)) {
    expect(translate("en-US", field.label)).not.toBe(field.label);
    expect(translate("ja-JP", field.label)).not.toBe(field.label);
  }
});
