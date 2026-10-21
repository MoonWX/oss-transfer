// The parent lifecycle is separate from scanning and child queue states.
export const parentTaskStates = [
  "RUNNING",
  "STOPPING",
  "RESUMING",
  "COMPLETED",
  "PARTIAL_FAILED",
  "FAILED",
  "STOPPED",
];

export function taskStatusGroup(status: string) {
  if (status === "LIST_DONE") return "RUNNING";
  if (status === "PENDING") return "WAITING";
  return status;
}

export function parentTaskFilterStates(
  tasks: { status: string }[],
  selected = "",
) {
  const present = new Set(tasks.map((task) => taskStatusGroup(task.status)));
  // Keep a selected compatibility filter stable across polling and history pages.
  if (selected) present.add(selected);
  return [
    ...parentTaskStates,
    ...["WAITING", "RECOVERABLE"].filter((state) => present.has(state)),
  ];
}
