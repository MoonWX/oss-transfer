package com.transfer.tools.agent;

import com.transfer.tools.config.DistributedConfig;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.core.io.FileSystemResource;
import org.springframework.http.*;
import org.springframework.web.bind.annotation.*;
import java.io.InputStream;
import java.nio.file.*;
import java.security.MessageDigest;
import java.util.*;

@RestController
public class AgentController {
    @Autowired private AgentRegistryService registry;
    @Autowired private AgentBroker broker;
    @Autowired private AgentRelayService relay;
    @Autowired private AgentSynchronizationService synchronization;
    @Autowired private DistributedConfig config;
    @Value("${transfer.agent.package-path:./agent-bundle.tar.gz}") private String packagePath;

    @PostMapping("/api/master/agents/invitations")
    public Map<String, Object> invite(@RequestBody(required = false) AgentRegistryService.InviteRequest request) {
        return registry.invite(request);
    }
    @PostMapping("/api/agents/join")
    public Map<String, Object> join(@RequestBody AgentRegistryService.JoinRequest request) { return registry.join(request); }
    @GetMapping("/api/agents/poll")
    public ResponseEntity<RpcCommand> poll(@RequestHeader(value = "X-Agent-Key", required = false) String credential) throws InterruptedException {
        String address = authenticate(credential); synchronization.connected(address);
        RpcCommand command = broker.poll(address, 25000);
        String ready = Boolean.toString(registry.online(address));
        return command == null ? ResponseEntity.noContent().header("X-Agent-Ready", ready).build()
                : ResponseEntity.ok().header("X-Agent-Ready", ready).body(command);
    }
    @PostMapping("/api/agents/result")
    public ResponseEntity<Void> result(@RequestHeader(value = "X-Agent-Key", required = false) String credential,
                                       @RequestBody RpcResult result) {
        String address = authenticate(credential); registry.touch(address); broker.complete(address, result);
        return ResponseEntity.noContent().build();
    }
    @PostMapping("/api/agents/relay")
    public RpcResponse relay(@RequestHeader(value = "X-Agent-Key", required = false) String credential,
                            @RequestBody RpcRelayRequest request) throws java.io.IOException { return relay.relay(authenticate(credential), request); }
    @ExceptionHandler(java.io.IOException.class)
    public ResponseEntity<Map<String, Object>> timeout() { return error(504, "Agent did not acknowledge the request; check task status before retrying"); }
    @GetMapping(value = "/api/agents/bootstrap/{token}", produces = "text/x-shellscript")
    public ResponseEntity<String> bootstrap(@PathVariable String token) {
        AgentRegistryService.Invitation invitation = registry.validInvitation(token);
        String origin = config.getMasterWebUrl(); String url = origin + "/api/agents/package/" + token;
        String script = "#!/usr/bin/env bash\nset -euo pipefail\numask 077\n"
                + "[[ ${EUID:-$(id -u)} -eq 0 ]] || { echo 'Run as root' >&2; exit 1; }\n"
                + "tmp=$(mktemp -d)\ntrap 'rm -rf \"$tmp\"' EXIT\n"
                + "curl --fail --silent --show-error " + AgentRegistryService.quote(url) + " -o \"$tmp/agent.tar.gz\"\n"
                + "curl --fail --silent --show-error " + AgentRegistryService.quote(url + "/sha256") + " -o \"$tmp/expected\"\n"
                + "actual=$(sha256sum \"$tmp/agent.tar.gz\" | awk '{print $1}')\n"
                + "[[ \"$actual\" = \"$(cat \"$tmp/expected\")\" ]] || { echo 'Package checksum mismatch' >&2; exit 1; }\n"
                + "mkdir \"$tmp/package\"\ntar -xzf \"$tmp/agent.tar.gz\" -C \"$tmp/package\"\n"
                + "bash \"$tmp/package/deploy/install-agent.sh\" --package-dir \"$tmp/package\""
                + " --master-url " + AgentRegistryService.quote(origin) + " --join-token " + AgentRegistryService.quote(token)
                + (invitation.getAddress().isEmpty() ? "" : " --address " + AgentRegistryService.quote(invitation.getAddress()))
                + (invitation.getLabel().isEmpty() ? "" : " --label " + AgentRegistryService.quote(invitation.getLabel())) + "\n";
        return ResponseEntity.ok().header("Cache-Control", "no-store").header("Referrer-Policy", "no-referrer").body(script);
    }
    @GetMapping("/api/agents/package/{token}")
    public ResponseEntity<FileSystemResource> download(@PathVariable String token) {
        registry.validInvitation(token); Path file = bundle();
        return ResponseEntity.ok().header("Cache-Control", "no-store")
                .header("Content-Disposition", "attachment; filename=agent-bundle.tar.gz")
                .contentType(MediaType.APPLICATION_OCTET_STREAM).body(new FileSystemResource(file.toFile()));
    }
    @GetMapping(value = "/api/agents/package/{token}/sha256", produces = "text/plain")
    public ResponseEntity<String> checksum(@PathVariable String token) throws Exception {
        registry.validInvitation(token); MessageDigest digest = MessageDigest.getInstance("SHA-256");
        try (InputStream stream = Files.newInputStream(bundle())) {
            byte[] buffer = new byte[65536]; int read;
            while ((read = stream.read(buffer)) >= 0) digest.update(buffer, 0, read);
        }
        StringBuilder hex = new StringBuilder(); for (byte value : digest.digest()) hex.append(String.format("%02x", value));
        return ResponseEntity.ok().header("Cache-Control", "no-store").body(hex + "\n");
    }
    private Path bundle() {
        Path path = Paths.get(packagePath).toAbsolutePath().normalize();
        if (!Files.isRegularFile(path) || !Files.isReadable(path))
            throw new IllegalStateException("Agent package is unavailable. Deploy the complete release package or official Docker image.");
        return path;
    }
    private String authenticate(String credential) {
        if (!config.isMaster()) throw new AgentUnauthorizedException();
        String address = registry.authenticatedAddress(credential);
        if (address == null) throw new AgentUnauthorizedException();
        return address;
    }
    static class AgentUnauthorizedException extends RuntimeException { }
    @ExceptionHandler(AgentUnauthorizedException.class)
    public ResponseEntity<Map<String, Object>> unauthorized() { return error(401, "Agent credential is invalid or revoked"); }
    @ExceptionHandler(IllegalArgumentException.class)
    public ResponseEntity<Map<String, Object>> invalid(IllegalArgumentException e) { return error(400, e.getMessage()); }
    @ExceptionHandler(SecurityException.class)
    public ResponseEntity<Map<String, Object>> forbidden() { return error(403, "Agent is not authorized for this command"); }
    @ExceptionHandler(IllegalStateException.class)
    public ResponseEntity<Map<String, Object>> unavailable(IllegalStateException e) { return error(503, e.getMessage()); }
    private ResponseEntity<Map<String, Object>> error(int status, String message) {
        Map<String, Object> result = new LinkedHashMap<>(); result.put("success", false); result.put("message", message);
        return ResponseEntity.status(status).header("Cache-Control", "no-store").body(result);
    }
}
