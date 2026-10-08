package com.transfer.tools.service;

import com.transfer.tools.model.RocksDBPage;
import com.transfer.tools.storage.RocksDBService;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Service;
import java.io.BufferedReader;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.*;

/** A durable input snapshot: restart must never turn an inventory into a bucket scan. */
@Service
public class InventoryManifestService {
    @Autowired private RocksDBService rocksDBService;
    public static class Entry {
        public String key;
        public Entry() { }
        Entry(String key) { this.key = key; }
    }
    public String create(Path file) throws java.io.IOException {
        String id = UUID.randomUUID().toString();
        String prefix = prefix(id);
        Map<String,Object> batch = new LinkedHashMap<>();
        try {
            try (BufferedReader reader = Files.newBufferedReader(file, StandardCharsets.UTF_8)) {
                String line;
                while ((line = reader.readLine()) != null) {
                    if (line.trim().isEmpty()) continue;
                    String key = prefix + Base64.getUrlEncoder().withoutPadding().encodeToString(line.getBytes(StandardCharsets.UTF_8));
                    batch.put(key, new Entry(line));
                    if (batch.size() >= 1000) { rocksDBService.putBatchSync(batch); batch.clear(); }
                }
            }
            if (!batch.isEmpty()) rocksDBService.putBatchSync(batch);
            rocksDBService.putSync("inventory:" + id + ":ready", true);
            return id;
        } catch (java.io.IOException | RuntimeException failure) {
            try {
                rocksDBService.deleteByPrefix("inventory:" + id + ":");
            } catch (RuntimeException cleanupFailure) {
                if (cleanupFailure != failure) failure.addSuppressed(cleanupFailure);
            }
            throw failure;
        }
    }
    public void requireReady(String id) {
        prefix(id);
        if (!Boolean.TRUE.equals(rocksDBService.get("inventory:" + id + ":ready", Boolean.class)))
            throw new IllegalStateException("清单快照不可用，无法继续任务；请重新导入原清单");
    }
    public RocksDBPage<Entry> page(String id, String cursor) {
        requireReady(id);
        return rocksDBService.scanPage(prefix(id), cursor, 500, Entry.class);
    }
    private String prefix(String id) {
        if (id == null || !id.matches("[0-9a-f-]{36}")) throw new IllegalArgumentException("清单快照标识无效");
        return "inventory:" + id + ":entry:";
    }
}
