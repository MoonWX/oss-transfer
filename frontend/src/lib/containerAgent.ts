// Container Agents are updated by recreating them from a new image with the
// same data volume.
//
// The console's Docker command (v4.2.0 and later) names each container
// oss-transfer-agent-<6 hex characters>, uses that name as the hostname the
// Agent reports, and keeps its data in the volume <name>-data. Containers
// added by earlier versions use the fixed names below.
const legacyContainer = "oss-transfer-agent";
const joinedContainer = /^oss-transfer-agent-[0-9a-f]{6}$/;
const agentCompose = "/opt/oss-transfer-compose/agent/docker-compose.yml";
const hardening =
  " --read-only --tmpfs /tmp:mode=1777 --cap-drop ALL --security-opt no-new-privileges --stop-timeout 45";

/** Commands that recreate a container Agent from the Master's image. */
export function containerAgentCommands(image: string, hostname?: string) {
  const joined = !!hostname && joinedContainer.test(hostname);
  const container = joined ? hostname : legacyContainer;
  const volume = `${container}-data`;
  const compose = `docker compose -p ${legacyContainer} -f ${agentCompose}`;
  return {
    run: [
      `docker pull ${image}`,
      `docker rm -f ${container}`,
      joined
        ? `docker run -d --name ${container} --hostname ${container} --restart unless-stopped${hardening} -v ${volume}:/data ${image} oss-transfer-agent --data-dir /data`
        : `docker run -d --name ${container} --restart unless-stopped -v ${volume}:/data ${image} oss-transfer-agent --data-dir /data`,
    ].join("\n"),
    compose: [`${compose} pull`, `${compose} up -d`].join("\n"),
  };
}
