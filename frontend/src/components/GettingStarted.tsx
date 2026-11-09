import { Avatar, Box, Button, Stack, Typography } from "@mui/material";
import { CheckRounded } from "@mui/icons-material";
import { Link as RouterLink } from "react-router-dom";
import { useApi } from "../lib/api";
import { t, useI18n } from "../lib/i18n";
import type { NodeRegistry } from "../lib/nodeRegistry";
import { formatNumber } from "./Common";

type Step = {
  title: string;
  description: string;
  // Undefined while the state is unknown or for steps without one.
  done?: boolean;
  state?: string;
  action?: { label: string; to: string };
};

/**
 * The first-run checklist: execution nodes, storage connections, a task and
 * its results, each with its current state where the console knows it.
 */
export default function GettingStarted({
  onNavigate,
}: {
  onNavigate?: () => void;
}) {
  useI18n();
  const registry = useApi<{ data?: NodeRegistry }>("/api/master/nodes");
  const storage = useApi<{ configs?: unknown[] }>("/api/master/s3-config");
  const nodes = registry.data?.data?.nodes?.length;
  const connections = storage.data?.configs?.length;
  const steps: Step[] = [
    {
      title: t("添加执行节点"),
      description: t(
        "迁移由执行节点（Agent）完成。在执行节点页生成加入命令，并在要运行 Agent 的主机上执行。",
      ),
      done: nodes === undefined ? undefined : nodes > 0,
      state:
        nodes === undefined
          ? undefined
          : nodes > 0
            ? t("已添加 {count} 个节点", { count: formatNumber(nodes) })
            : t("尚未添加节点"),
      action: { label: t("添加节点"), to: "/nodes?add=1" },
    },
    {
      title: t("添加存储连接"),
      description: t("配置 S3 兼容存储的访问地址和凭证。"),
      done: connections === undefined ? undefined : connections > 0,
      state:
        connections === undefined
          ? undefined
          : connections > 0
            ? t("已添加 {count} 个连接", { count: formatNumber(connections) })
            : t("尚未添加连接"),
      action: { label: t("管理存储连接"), to: "/storage" },
    },
    {
      title: t("创建迁移任务"),
      description: t(
        "配置传输方向、来源和目标。本地传输需绑定执行节点并通过路径预检。",
      ),
      action: { label: t("创建迁移任务"), to: "/create" },
    },
    {
      title: t("执行与查看结果"),
      description: t("任务创建后自动执行。结果见任务详情和异常记录。"),
    },
  ];
  return (
    <Stack component="ol" gap={3} sx={{ m: 0, p: 0, listStyle: "none" }}>
      {steps.map((step, index) => (
        <Stack key={step.title} component="li" direction="row" gap={2}>
          <Avatar
            sx={{
              bgcolor: step.done ? "success.main" : "action.selected",
              color: step.done ? "success.contrastText" : "primary.main",
            }}
          >
            {step.done ? <CheckRounded aria-hidden /> : index + 1}
          </Avatar>
          <Box sx={{ minWidth: 0 }}>
            <Typography fontWeight={500}>{step.title}</Typography>
            <Typography variant="body2" color="text.secondary">
              {step.description}
            </Typography>
            {(step.state || step.action) && (
              <Stack
                direction="row"
                alignItems="center"
                gap={1.5}
                flexWrap="wrap"
                sx={{ mt: 0.5 }}
              >
                {step.state && (
                  <Typography
                    variant="caption"
                    color={step.done ? "success.main" : "warning.main"}
                  >
                    {step.state}
                  </Typography>
                )}
                {step.action && (
                  <Button
                    size="small"
                    component={RouterLink}
                    to={step.action.to}
                    onClick={onNavigate}
                    sx={{ px: 0.5, minWidth: 0 }}
                  >
                    {step.action.label}
                  </Button>
                )}
              </Stack>
            )}
          </Box>
        </Stack>
      ))}
    </Stack>
  );
}
