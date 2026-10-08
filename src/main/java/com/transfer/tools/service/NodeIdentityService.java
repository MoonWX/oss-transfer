package com.transfer.tools.service;

import com.transfer.tools.config.DistributedConfig;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Service;
import javax.crypto.Mac;
import javax.crypto.spec.SecretKeySpec;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.util.*;

/** A one-shot challenge proves possession without sending the cluster secret to a candidate. */
@Service
public class NodeIdentityService {
    @Autowired private DistributedConfig config;

    public Map<String, Object> prove(String nonce) {
        if (nonce == null || !nonce.matches("[a-f0-9]{64}"))
            throw new TransferValidationException("INVALID_NODE_CHALLENGE", "节点校验请求无效");
        Map<String, Object> proof = new LinkedHashMap<>();
        proof.put("nonce", nonce); proof.put("localIp", config.getLocalIp());
        proof.put("masterIp", config.getMasterIp()); proof.put("port", config.getPort());
        proof.put("worker", config.isWorker()); proof.put("master", config.isMaster());
        proof.put("signature", sign(proof, config.getInternalToken()));
        return proof;
    }

    static String sign(Map<String, Object> proof, String token) {
        if (token == null || token.isEmpty())
            throw new TransferValidationException("NODE_TOKEN_MISSING", "节点未配置集群内部密钥");
        String payload = "oss-transfer-node-v1\n" + proof.get("nonce") + "\n" + proof.get("localIp")
                + "\n" + proof.get("masterIp") + "\n" + proof.get("port")
                + "\n" + proof.get("worker") + "\n" + proof.get("master");
        try {
            Mac mac = Mac.getInstance("HmacSHA256");
            mac.init(new SecretKeySpec(token.getBytes(StandardCharsets.UTF_8), "HmacSHA256"));
            return Base64.getEncoder().encodeToString(mac.doFinal(payload.getBytes(StandardCharsets.UTF_8)));
        } catch (Exception e) { throw new IllegalStateException("节点身份签名不可用", e); }
    }

    static boolean valid(Map<String, Object> proof, String nonce, String address, DistributedConfig config) {
        if (proof == null || !nonce.equals(proof.get("nonce")) || !address.equals(proof.get("localIp"))
                || !Objects.equals(config.getMasterIp(), proof.get("masterIp"))
                || !String.valueOf(config.getPort()).equals(String.valueOf(proof.get("port")))
                || !Boolean.TRUE.equals(proof.get("worker"))
                || !Objects.equals(address.equals(config.getMasterIp()), proof.get("master"))) return false;
        Object signature = proof.get("signature");
        return signature instanceof String && MessageDigest.isEqual(
                sign(proof, config.getInternalToken()).getBytes(StandardCharsets.UTF_8),
                ((String) signature).getBytes(StandardCharsets.UTF_8));
    }
}
