import {
  Alert,
  Box,
  Button,
  Chip,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  LinearProgress,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  Typography,
} from "@mui/material";
import { useApi } from "../lib/api";
import { formatDate, t, useI18n } from "../lib/i18n";
import { nodeName, type RegisteredNode } from "../lib/nodeRegistry";

type NodeDetails = RegisteredNode & {
  connectionAddress?: string;
  hostname?: string;
  os?: string;
  arch?: string;
  localIps?: string[];
  mounts?: {
    path: string;
    source: string;
    fileSystem: string;
    readOnly: boolean;
  }[];
  complete: boolean;
};

export default function NodeDetailsDialog({
  node,
  onClose,
}: {
  node: RegisteredNode;
  onClose: () => void;
}) {
  useI18n();
  const result = useApi<{ data: NodeDetails }>(
    `/api/master/nodes/${encodeURIComponent(node.address)}/details`,
  );
  const details = result.data?.data;
  const fields = [
    [t("主机名"), details?.hostname],
    [
      t("系统 / 架构"),
      [details?.os, details?.arch].filter(Boolean).join(" / "),
    ],
    [t("网卡 IP"), details?.localIps?.join(", ")],
    [t("连接来源 IP"), details?.connectionAddress],
    [
      t("最后连接"),
      details?.lastSeen ? formatDate(details.lastSeen) : undefined,
    ],
    [t("内部 ID"), details?.id ?? node.id],
  ];
  return (
    <Dialog
      open
      fullWidth
      maxWidth="md"
      onClose={onClose}
      aria-labelledby="node-details-title"
    >
      <DialogTitle id="node-details-title">
        {t("节点详情")} · {nodeName(details ?? node)}
      </DialogTitle>
      <DialogContent>
        <Stack spacing={2}>
          {result.loading && <LinearProgress />}
          {result.error && <Alert severity="error">{result.error}</Alert>}
          {details && (
            <>
              <Chip
                label={details.online ? t("在线") : t("离线")}
                size="small"
                color={details.online ? "success" : "default"}
                sx={{ alignSelf: "flex-start" }}
              />
              {!details.complete && (
                <Alert severity="info">{t("节点未返回完整信息。")}</Alert>
              )}
              <Box
                component="dl"
                sx={{
                  display: "grid",
                  gridTemplateColumns: {
                    xs: "1fr",
                    sm: "150px minmax(0, 1fr)",
                  },
                  columnGap: 2,
                  rowGap: 1.5,
                  m: 0,
                }}
              >
                {fields
                  .filter(([, value]) => !!value)
                  .map(([label, value]) => (
                    <Box key={label} sx={{ display: "contents" }}>
                      <Typography component="dt" color="text.secondary">
                        {label}
                      </Typography>
                      <Typography
                        component="dd"
                        sx={{
                          m: 0,
                          whiteSpace: "pre-wrap",
                          overflowWrap: "anywhere",
                        }}
                      >
                        {value}
                      </Typography>
                    </Box>
                  ))}
              </Box>
              <Typography variant="caption" color="text.secondary">
                {t(
                  "连接来源为 Master 观测到的地址，可能是内网、NAT 或代理地址。",
                )}
              </Typography>
              {!!details.mounts?.length && (
                <>
                  <Typography variant="subtitle1">{t("挂载目录")}</Typography>
                  <TableContainer>
                    <Table size="small" aria-label={t("挂载目录")}>
                      <TableHead>
                        <TableRow>
                          {[t("目录"), t("来源"), t("文件系统"), t("权限")].map(
                            (label) => (
                              <TableCell key={label}>{label}</TableCell>
                            ),
                          )}
                        </TableRow>
                      </TableHead>
                      <TableBody>
                        {details.mounts.map((mount, index) => (
                          <TableRow key={`${mount.path}-${index}`}>
                            <TableCell sx={{ overflowWrap: "anywhere" }}>
                              {mount.path}
                            </TableCell>
                            <TableCell sx={{ overflowWrap: "anywhere" }}>
                              {mount.source}
                            </TableCell>
                            <TableCell>{mount.fileSystem}</TableCell>
                            <TableCell>
                              {mount.readOnly ? t("只读") : t("读写")}
                            </TableCell>
                          </TableRow>
                        ))}
                      </TableBody>
                    </Table>
                  </TableContainer>
                </>
              )}
            </>
          )}
        </Stack>
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>{t("关闭")}</Button>
      </DialogActions>
    </Dialog>
  );
}
