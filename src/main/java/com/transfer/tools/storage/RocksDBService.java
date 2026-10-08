package com.transfer.tools.storage;

import com.alibaba.fastjson.JSON;
import lombok.extern.slf4j.Slf4j;
import org.rocksdb.Options;
import org.rocksdb.RocksDB;
import org.rocksdb.RocksDBException;
import org.rocksdb.RocksIterator;
import org.rocksdb.WriteBatch;
import org.rocksdb.WriteOptions;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.stereotype.Service;

import javax.annotation.PostConstruct;
import javax.annotation.PreDestroy;
import java.io.File;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

@Slf4j
@Service
public class RocksDBService {

    private RocksDB rocksDB;

    @Value("${rocksdb.path:./data/rocksdb}")
    private String dbPath;

    @PostConstruct
    public void init() {
        try {
            RocksDB.loadLibrary();

            File dir = new File(dbPath);
            if (!dir.exists()) {
                dir.mkdirs();
            }

            Options options = new Options();
            options.setCreateIfMissing(true);

            rocksDB = RocksDB.open(options, dbPath);
            log.info("RocksDB初始化成功，路径: {}", dbPath);
        } catch (RocksDBException e) {
            log.error("RocksDB初始化失败", e);
            throw new RuntimeException("RocksDB初始化失败", e);
        }
    }

    @PreDestroy
    public void destroy() {
        if (rocksDB != null) {
            rocksDB.close();
            log.info("RocksDB关闭成功");
        }
    }

    public void put(String key, Object value) {
        try {
            String jsonValue = JSON.toJSONString(value);
            rocksDB.put(key.getBytes(StandardCharsets.UTF_8), jsonValue.getBytes(StandardCharsets.UTF_8));
        } catch (RocksDBException e) {
            log.error("RocksDB写入失败, key: {}", key, e);
            throw new RuntimeException("RocksDB写入失败", e);
        }
    }

    /** Persist acknowledged settings to the WAL before applying them in memory. */
    public void putSync(String key, Object value) {
        try (WriteOptions options = new WriteOptions().setSync(true)) {
            rocksDB.put(options, key.getBytes(StandardCharsets.UTF_8),
                    JSON.toJSONString(value).getBytes(StandardCharsets.UTF_8));
        } catch (RocksDBException e) {
            log.error("RocksDB同步写入失败, key: {}", key, e);
            throw new RuntimeException("RocksDB同步写入失败", e);
        }
    }

    public <T> T get(String key, Class<T> clazz) {
        try {
            byte[] bytes = rocksDB.get(key.getBytes(StandardCharsets.UTF_8));
            if (bytes == null) {
                return null;
            }
            String jsonValue = new String(bytes, StandardCharsets.UTF_8);
            return JSON.parseObject(jsonValue, clazz);
        } catch (RocksDBException e) {
            log.error("RocksDB读取失败, key: {}", key, e);
            throw new RuntimeException("RocksDB读取失败", e);
        }
    }

    public void delete(String key) {
        try {
            rocksDB.delete(key.getBytes(StandardCharsets.UTF_8));
        } catch (RocksDBException e) {
            log.error("RocksDB删除失败, key: {}", key, e);
            throw new RuntimeException("RocksDB删除失败", e);
        }
    }

    public boolean exists(String key) {
        try {
            byte[] bytes = rocksDB.get(key.getBytes(StandardCharsets.UTF_8));
            return bytes != null;
        } catch (RocksDBException e) {
            log.error("RocksDB查询失败, key: {}", key, e);
            return false;
        }
    }

    public <T> List<T> scanByPrefix(String prefix, Class<T> clazz) {
        List<T> results = new ArrayList<>();
        try {
            RocksIterator iterator = rocksDB.newIterator();
            byte[] prefixBytes = prefix.getBytes(StandardCharsets.UTF_8);

            for (iterator.seek(prefixBytes); iterator.isValid(); iterator.next()) {
                byte[] keyBytes = iterator.key();
                String key = new String(keyBytes, StandardCharsets.UTF_8);

                if (!key.startsWith(prefix)) {
                    break;
                }

                byte[] valueBytes = iterator.value();
                String jsonValue = new String(valueBytes, StandardCharsets.UTF_8);
                T obj = JSON.parseObject(jsonValue, clazz);
                if (obj != null) {
                    results.add(obj);
                }
            }

            iterator.close();
        } catch (Exception e) {
            log.error("RocksDB扫描失败, prefix: {}", prefix, e);
        }
        return results;
    }

    public void putBatch(Map<String, Object> dataMap) {
        if (dataMap == null || dataMap.isEmpty()) {
            return;
        }
        try (WriteBatch batch = new WriteBatch()) {
            for (Map.Entry<String, Object> entry : dataMap.entrySet()) {
                String key = entry.getKey();
                String jsonValue = JSON.toJSONString(entry.getValue());
                batch.put(key.getBytes(StandardCharsets.UTF_8), jsonValue.getBytes(StandardCharsets.UTF_8));
            }
            rocksDB.write(new WriteOptions(), batch);
        } catch (RocksDBException e) {
            log.error("RocksDB批量写入失败, size: {}", dataMap.size(), e);
            throw new RuntimeException("RocksDB批量写入失败", e);
        }
    }

    /** Atomic WAL-synced updates for report records and their coverage metadata. */
    public void putBatchSync(Map<String, Object> values) {
        if (values == null || values.isEmpty()) return;
        try (WriteBatch batch = new WriteBatch(); WriteOptions options = new WriteOptions().setSync(true)) {
            for (Map.Entry<String, Object> entry : values.entrySet()) {
                batch.put(entry.getKey().getBytes(StandardCharsets.UTF_8),
                        JSON.toJSONBytes(entry.getValue()));
            }
            rocksDB.write(options, batch);
        } catch (RocksDBException e) {
            throw new IllegalStateException("RocksDB synchronous batch write failed", e);
        }
    }

    /** Bounded, exclusive cursor iteration. The cursor is scoped to the exact prefix. */
    public <T> com.transfer.tools.model.RocksDBPage<T> scanPage(
            String prefix, String cursor, int limit, Class<T> clazz) {
        if (prefix == null || prefix.isEmpty() || limit < 1 || limit > 1000) {
            throw new IllegalArgumentException("prefix and limit (1..1000) are required");
        }
        String after = null;
        if (cursor != null && !cursor.isEmpty()) {
            try {
                after = new String(java.util.Base64.getUrlDecoder().decode(cursor), StandardCharsets.UTF_8);
            } catch (IllegalArgumentException e) {
                throw new IllegalArgumentException("Invalid report cursor", e);
            }
            if (!after.startsWith(prefix)) throw new IllegalArgumentException("Cursor belongs to a different scope");
        }
        com.transfer.tools.model.RocksDBPage<T> page = new com.transfer.tools.model.RocksDBPage<>();
        String last = null;
        try (RocksIterator iterator = rocksDB.newIterator()) {
            iterator.seek((after == null ? prefix : after).getBytes(StandardCharsets.UTF_8));
            if (after != null && iterator.isValid()
                    && after.equals(new String(iterator.key(), StandardCharsets.UTF_8))) iterator.next();
            while (iterator.isValid()) {
                String key = new String(iterator.key(), StandardCharsets.UTF_8);
                if (!key.startsWith(prefix)) break;
                if (page.getItems().size() == limit) break;
                T value = JSON.parseObject(new String(iterator.value(), StandardCharsets.UTF_8), clazz);
                if (value == null) throw new IllegalStateException("Null report entry: " + key);
                page.getItems().put(key, value);
                last = key;
                iterator.next();
            }
            iterator.status();
            boolean more = iterator.isValid()
                    && new String(iterator.key(), StandardCharsets.UTF_8).startsWith(prefix);
            page.setDone(!more);
            if (more && last != null) page.setNextCursor(java.util.Base64.getUrlEncoder().withoutPadding()
                    .encodeToString(last.getBytes(StandardCharsets.UTF_8)));
            return page;
        } catch (RocksDBException e) {
            throw new IllegalStateException("RocksDB cursor scan failed", e);
        }
    }

    public void deleteBatch(List<String> keys) {
        if (keys == null || keys.isEmpty()) {
            return;
        }
        try (WriteBatch batch = new WriteBatch()) {
            for (String key : keys) {
                batch.delete(key.getBytes(StandardCharsets.UTF_8));
            }
            rocksDB.write(new WriteOptions(), batch);
        } catch (RocksDBException e) {
            log.error("RocksDB批量删除失败, size: {}", keys.size(), e);
            throw new RuntimeException("RocksDB批量删除失败", e);
        }
    }

    /** Remove only database records in an exact namespace, in bounded batches. */
    public void deleteByPrefix(String prefix) {
        if (prefix == null || prefix.isEmpty()) throw new IllegalArgumentException("A non-empty prefix is required");
        String cursor = null;
        while (true) {
            com.transfer.tools.model.RocksDBPage<Object> page = scanPage(prefix, cursor, 1000, Object.class);
            deleteBatch(new ArrayList<>(page.getItems().keySet()));
            if (page.isDone()) return;
            cursor = page.getNextCursor();
        }
    }

    public <T> Map<String, T> multiGet(List<String> keys, Class<T> clazz) {
        Map<String, T> results = new HashMap<>();
        if (keys == null || keys.isEmpty()) {
            return results;
        }
        try {
            List<byte[]> keyBytes = new ArrayList<>();
            for (String key : keys) {
                keyBytes.add(key.getBytes(StandardCharsets.UTF_8));
            }
            List<byte[]> values = rocksDB.multiGetAsList(keyBytes);
            for (int i = 0; i < keys.size(); i++) {
                byte[] value = values.get(i);
                if (value != null) {
                    String key = keys.get(i);
                    String jsonValue = new String(value, StandardCharsets.UTF_8);
                    T obj = JSON.parseObject(jsonValue, clazz);
                    if (obj != null) {
                        results.put(key, obj);
                    }
                }
            }
        } catch (RocksDBException e) {
            log.error("RocksDB批量读取失败, size: {}", keys.size(), e);
            throw new RuntimeException("RocksDB批量读取失败", e);
        }
        return results;
    }

    public void putBatchWithDeletes(Map<String, Object> puts, List<String> deletes) {
        try (WriteBatch batch = new WriteBatch()) {
            if (puts != null) {
                for (Map.Entry<String, Object> entry : puts.entrySet()) {
                    String key = entry.getKey();
                    String jsonValue = JSON.toJSONString(entry.getValue());
                    batch.put(key.getBytes(StandardCharsets.UTF_8), jsonValue.getBytes(StandardCharsets.UTF_8));
                }
            }
            if (deletes != null) {
                for (String key : deletes) {
                    batch.delete(key.getBytes(StandardCharsets.UTF_8));
                }
            }
            rocksDB.write(new WriteOptions(), batch);
        } catch (RocksDBException e) {
            log.error("RocksDB批量写入(含删除)失败", e);
            throw new RuntimeException("RocksDB批量写入(含删除)失败", e);
        }
    }
}
