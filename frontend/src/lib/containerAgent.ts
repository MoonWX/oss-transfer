// The names the Docker join command and the installer's Compose method use.
const agentContainer = "oss-transfer-agent";
const agentVolume = "oss-transfer-agent-data";
const agentCompose = "/opt/oss-transfer-compose/agent/docker-compose.yml";

/** Commands that recreate a container Agent from the Master's image. */
export function containerAgentCommands(image: string) {
  const compose = `docker compose -p ${agentContainer} -f ${agentCompose}`;
  return {
    run: [
      `docker pull ${image}`,
      `docker rm -f ${agentContainer}`,
      `docker run -d --name ${agentContainer} --restart unless-stopped -v ${agentVolume}:/data ${image} oss-transfer-agent --data-dir /data`,
    ].join("\n"),
    compose: [`${compose} pull`, `${compose} up -d`].join("\n"),
  };
}
