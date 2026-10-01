import React from "react";
import {
  Content,
  Flex,
  FlexItem,
  Icon,
  Label,
  Stack,
  StackItem,
  Tooltip,
} from "@patternfly/react-core";
import CheckCircleIcon from "@patternfly/react-icons/dist/esm/icons/check-circle-icon";
import TimesCircleIcon from "@patternfly/react-icons/dist/esm/icons/times-circle-icon";
import type { ActivityEntry } from "../types";

function relativeTime(iso: string): string {
  const now = Date.now();
  const then = new Date(iso).getTime();
  const diffSec = Math.floor((now - then) / 1000);
  if (diffSec < 60) return "just now";
  const diffMin = Math.floor(diffSec / 60);
  if (diffMin < 60) return `${diffMin}m ago`;
  const diffHr = Math.floor(diffMin / 60);
  if (diffHr < 24) return `${diffHr}h ago`;
  const diffDay = Math.floor(diffHr / 24);
  return `${diffDay}d ago`;
}

function actionLabel(action: string): string {
  switch (action) {
    case "update": return "Updated to nightly";
    case "refresh": return "Operator refreshed";
    case "rollback":
    case "reinstall": return "Operator reinstalled";
    case "deploy-pr": return "Dashboard PR deployed";
    case "deploy-dashboard-pr": return "Dashboard PR deployed";
    case "deploy-dashboard-main": return "Dashboard main deployed";
    case "revert-dashboard": return "Dashboard reverted";
    case "create-pull-secret": return "Pull secret configured";
    case "setup-minio": return "MinIO set up";
    case "teardown-minio": return "MinIO torn down";
    case "setup-pipeline-server": return "Pipeline server set up";
    case "teardown-pipeline-server": return "Pipeline server torn down";
    case "setup-mlflow": return "MLflow set up";
    case "teardown-mlflow": return "MLflow torn down";
    case "deploy-mlflow-pr": return "MLflow PR deployed";
    case "revert-mlflow": return "MLflow reverted";
    case "assist-rollout": return "Rollout assisted";
    default: return action;
  }
}

function actionColor(action: string): "blue" | "orange" | "teal" | "purple" | "grey" | "red" | "green" | "yellow" {
  switch (action) {
    case "update": return "blue";
    case "refresh": return "teal";
    case "rollback":
    case "reinstall": return "orange";
    case "deploy-pr": return "teal";
    case "deploy-dashboard-pr":
    case "deploy-dashboard-main": return "teal";
    case "revert-dashboard": return "orange";
    case "create-pull-secret": return "purple";
    case "setup-minio": return "green";
    case "teardown-minio": return "red";
    case "setup-pipeline-server": return "green";
    case "teardown-pipeline-server": return "red";
    case "setup-mlflow": return "green";
    case "teardown-mlflow": return "red";
    case "deploy-mlflow-pr": return "teal";
    case "revert-mlflow": return "orange";
    case "assist-rollout": return "yellow";
    default: return "grey";
  }
}

function formatDetail(action: string, detail: string): string {
  if (!detail) return "";
  if (action === "update") {
    const tagMatch = detail.match(/:([^@]+)/);
    return tagMatch ? tagMatch[1] : detail;
  }
  if (action === "refresh") {
    const versionMatch = detail.match(/\.(\d+\.\d+\.\d+-?\S*)/);
    return versionMatch ? `v${versionMatch[1]}` : detail;
  }
  return detail;
}

interface ActivityLogProps {
  activity?: ActivityEntry[];
}

export const ActivityLog: React.FC<ActivityLogProps> = React.memo(({ activity }) => {
  if (!activity || activity.length === 0) {
    return <Content component="small">No recent activity</Content>;
  }

  const maxDisplay = 5;
  const total = activity.length;
  const entries = [...activity].reverse().slice(0, maxDisplay);

  return (
    <Stack hasGutter style={{ minWidth: 0, maxWidth: "100%" }}>
      {entries.map((entry, i) => {
        const detail = formatDetail(entry.action, entry.detail);
        return (
          <StackItem key={i}>
            <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }} flexWrap={{ default: "wrap" }}>
              <FlexItem>
                <Icon isInline status={entry.success ? "success" : "danger"} size="sm">
                  {entry.success ? <CheckCircleIcon /> : <TimesCircleIcon />}
                </Icon>
              </FlexItem>
              <FlexItem>
                <Label isCompact color={actionColor(entry.action)}>{actionLabel(entry.action)}</Label>
              </FlexItem>
              <FlexItem>
                <Content component="small">{entry.user}</Content>
              </FlexItem>
              <FlexItem>
                <Tooltip content={new Date(entry.timestamp).toLocaleString()}>
                  <Content component="small">{relativeTime(entry.timestamp)}</Content>
                </Tooltip>
              </FlexItem>
            </Flex>
            {detail && (
              <Content component="small" style={{ overflowWrap: "anywhere" }} className="pf-v6-u-mt-xs">
                <Tooltip content={entry.detail}><span>{detail}</span></Tooltip>
              </Content>
            )}
          </StackItem>
        );
      })}
      {total > maxDisplay && (
        <StackItem>
          <Content component="small">Showing {maxDisplay} of {total}</Content>
        </StackItem>
      )}
    </Stack>
  );
});
