package com.transfer.tools.controller;

import com.transfer.tools.service.NodeIdentityService;
import com.transfer.tools.service.TransferValidationException;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.*;
import java.util.*;

@RestController
public class NodeIdentityController {
    @Autowired private NodeIdentityService identities;
    @GetMapping("/api/node-registration/proof")
    public ResponseEntity<Map<String, Object>> proof(@RequestParam String nonce) {
        try { return ResponseEntity.ok(identities.prove(nonce)); }
        catch (TransferValidationException e) {
            Map<String, Object> result = new HashMap<>(); result.put("success", false);
            result.put("message", "节点身份校验不可用");
            return ResponseEntity.badRequest().body(result);
        }
    }
}
