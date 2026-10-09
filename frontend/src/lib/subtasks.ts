type ExecutionSubtask = Record<string, any> & {
  taskIndex?: number;
  shardIndex?: number | null;
  shardId?: string;
  subtaskNumber?: number;
};

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
