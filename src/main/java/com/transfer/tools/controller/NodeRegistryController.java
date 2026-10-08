package com.transfer.tools.controller;

import com.transfer.tools.service.NodeRegistryService;
import com.transfer.tools.service.TransferValidationException;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.*;
import java.util.*;
import java.util.concurrent.Callable;

@RestController
@RequestMapping("/api/master/nodes")
public class NodeRegistryController {
    @Autowired private NodeRegistryService registry;
    @GetMapping public ResponseEntity<Map<String, Object>> list() { return respond(registry::list); }
    @PostMapping public ResponseEntity<Map<String, Object>> create(@RequestBody NodeRegistryService.Change change) { return respond(() -> registry.create(change)); }
    @PutMapping("/{address:.+}") public ResponseEntity<Map<String, Object>> update(@PathVariable String address, @RequestBody NodeRegistryService.Change change) { return respond(() -> registry.update(address, change)); }
    @DeleteMapping("/{address:.+}") public ResponseEntity<Map<String, Object>> delete(@PathVariable String address) { return respond(() -> registry.delete(address)); }
    private ResponseEntity<Map<String, Object>> respond(Callable<Map<String, Object>> action) {
        Map<String, Object> response = new LinkedHashMap<>();
        try {
            response.put("data", action.call()); response.put("success", true);
            return ResponseEntity.ok(response);
        } catch (TransferValidationException e) {
            response.put("success", false); response.put("code", e.getCode()); response.put("message", e.getMessage());
            return ResponseEntity.status(409).body(response);
        } catch (Exception e) {
            response.put("success", false); response.put("message", "节点配置未能保存，请检查节点连接或存储状态");
            return ResponseEntity.status(500).body(response);
        }
    }
}
