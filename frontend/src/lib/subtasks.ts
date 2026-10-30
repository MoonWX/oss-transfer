type ExecutionSubtask = Record<string, any> & {
  taskIndex?: number;
  shardIndex?: number | null;
  shardId?: string;
  subtaskNumber?: number;
};

// Keep active transfers visible before failures, queued work and history.
export function allSubtaskPriority(task: ExecutionSubtask): number {
  switch (task.status) {
    case "RUNNING":
    case "SCAN_COMPLETED":
      return 0;
    case "FAILED":
      return 1;
    case "PARTIAL_FAILED":
      return 2;
    case "RECOVERABLE":
    case "WAITING_RECOVERY":
      return 3;
    case "RECOVERING":
    case "RESUMING":
      return 4;
    case "STOPPING":
      return 5;
    case "WAITING":
    case "PENDING":
    case "CREATED":
      return 6;
    case "STOPPED":
      return 7;
    case "COMPLETED":
      return 9;
    case "DELETED":
      return 10;
    default:
      return 8;
  }
}

export function allSubtaskCursor(task: ExecutionSubtask): string {
  return `${String(allSubtaskPriority(task)).padStart(2, "0")}/${String(task.subtaskNumber).padStart(10, "0")}`;
}

export function compareAllSubtasks(
  a: ExecutionSubtask,
  b: ExecutionSubtask,
): number {
  return (
    allSubtaskPriority(a) - allSubtaskPriority(b) ||
    (a.subtaskNumber ?? 0) - (b.subtaskNumber ?? 0)
  );
}

export function activeSubtaskPriority(task: ExecutionSubtask): number {
  switch (task.status) {
    case "RUNNING":
    case "SCAN_COMPLETED":
      return 0;
    case "RECOVERING":
    case "RESUMING":
      return 1;
    case "STOPPING":
      return 2;
    case "RECOVERABLE":
    case "WAITING_RECOVERY":
      return 3;
    case "WAITING":
    case "PENDING":
    case "CREATED":
      return 4;
    default:
      return 5;
  }
}

export function activeSubtaskCursor(task: ExecutionSubtask): string {
  return `${activeSubtaskPriority(task)}/${String(task.subtaskNumber).padStart(10, "0")}`;
}

export function compareActiveSubtasks(
  a: ExecutionSubtask,
  b: ExecutionSubtask,
): number {
  return (
    activeSubtaskPriority(a) - activeSubtaskPriority(b) ||
    (a.subtaskNumber ?? 0) - (b.subtaskNumber ?? 0)
  );
}

// Keep the worker identity unchanged; the displayed number belongs to its parent.
export function numberSubtasks<T extends ExecutionSubtask>(
  tasks: T[],
  childTaskIds: number[] | null = [],
): Array<T & { subtaskNumber: number }> {
  const indexOf = (task: T): number | undefined => {
    if (Number.isInteger(task.subtaskNumber) && task.subtaskNumber! >= 1)
      return task.subtaskNumber! - 1;
    if (Number.isInteger(task.shardIndex) && task.shardIndex! >= 0)
      return task.shardIndex!;
    const legacyIndex = task.shardId?.match(
      /(?:-oss-|-local-|batch-|^oss-.+-)(\d+)$/,
    );
    if (legacyIndex) return Number(legacyIndex[1]);
    const position = childTaskIds?.indexOf(task.taskIndex!) ?? -1;
    return position >= 0 ? position : undefined;
  };
  const sorted = [...tasks].sort((a, b) => {
    const ai = indexOf(a),
      bi = indexOf(b);
    if (ai !== bi)
      return (ai ?? Number.MAX_SAFE_INTEGER) - (bi ?? Number.MAX_SAFE_INTEGER);
    return String(a.shardId || a.taskIndex).localeCompare(
      String(b.shardId || b.taskIndex),
      undefined,
      { numeric: true },
    );
  });
  const occupied = new Set(
    sorted.flatMap((task) =>
      indexOf(task) == null ? [] : [indexOf(task)! + 1],
    ),
  );
  let next = 1;
  return sorted.map((task) => {
    let number = indexOf(task);
    if (number != null) return { ...task, subtaskNumber: number + 1 };
    while (occupied.has(next)) next++;
    number = next++;
    occupied.add(number);
    return { ...task, subtaskNumber: number };
  });
}
