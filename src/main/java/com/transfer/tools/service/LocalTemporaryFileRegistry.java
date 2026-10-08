package com.transfer.tools.service;

import com.transfer.tools.model.RocksDBPage;
import com.transfer.tools.storage.RocksDBService;
import lombok.extern.slf4j.Slf4j;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Service;

import javax.annotation.PostConstruct;
import java.io.IOException;
import java.io.OutputStream;
import java.lang.management.ManagementFactory;
import java.nio.file.*;
import java.nio.file.attribute.BasicFileAttributes;
import java.nio.file.attribute.FileAttribute;
import java.nio.file.attribute.PosixFilePermissions;
import java.nio.channels.Channels;
import java.util.Arrays;
import java.util.HashSet;
import java.util.Set;
import java.util.UUID;

/** Exact ownership records, deliberately outside task/report namespaces removed by task deletion. */
@Service
@Slf4j
public class LocalTemporaryFileRegistry {
    static final String PREFIX = "local-temp:";
    @Autowired private RocksDBService rocksDBService;
    private final String processOwner = ManagementFactory.getRuntimeMXBean().getName() + ":"
            + ManagementFactory.getRuntimeMXBean().getStartTime() + ":" + UUID.randomUUID();
    private final Set<String> active = new HashSet<>();

    public static class Entry {
        public String id;
        public String root;
        public String rootFileKey;
        public String temp;
        public String target;
        public String logicalKey;
        public Integer taskId;
        public String owner;
        public String stage;
        public String fileKey;
    }

    /**
     * Dependency initialization completes before LocalFileService and its recovery/executor clients.
     * A failed database scan fails initialization; it must not silently skip the recovery barrier.
     */
    @PostConstruct
    public synchronized void recoverInterrupted() {
        String cursor = null;
        while (true) {
            RocksDBPage<Entry> page = rocksDBService.scanPage(PREFIX, cursor, 250, Entry.class);
            for (java.util.Map.Entry<String, Entry> item : page.getItems().entrySet()) {
                Entry entry = item.getValue();
                if (entry == null || entry.id == null || !item.getKey().equals(PREFIX + entry.id)) {
                    log.warn("保留无效临时文件登记: {}", item.getKey());
                    continue;
                }
                if (active.contains(entry.id) || processOwner.equals(entry.owner)) continue;
                try { cleanup(entry); }
                catch (IOException | RuntimeException failure) {
                    log.warn("保留无法安全清理的临时文件: {}", entry.temp, failure);
                }
            }
            if (page.isDone()) return;
            cursor = page.getNextCursor();
        }
    }

    /** target may be null for an owned upload spool; no operation ever deletes target. */
    public synchronized OwnedFile create(Path rootPath, Path target, String logicalKey, Integer taskId) throws IOException {
        Path root = rootPath.toRealPath();
        BasicFileAttributes rootAttributes = attributes(root);
        if (!rootAttributes.isDirectory() || rootAttributes.fileKey() == null) {
            throw new IOException("temporary-file root has no stable directory identity");
        }
        Path destination = target == null ? null : target.toAbsolutePath().normalize();
        Path parent = destination == null ? root : destination.getParent();
        requireSafeChain(root, parent);
        Entry entry = new Entry();
        entry.id = UUID.randomUUID().toString();
        entry.root = root.toString();
        entry.rootFileKey = rootAttributes.fileKey().toString();
        entry.temp = parent.resolve(".transfer-" + (taskId == null ? "unknown" : taskId)
                + "-" + entry.id + ".part").toString();
        entry.target = destination == null ? null : destination.toString();
        entry.logicalKey = logicalKey;
        entry.taskId = taskId;
        entry.owner = processOwner;
        entry.stage = "PLANNED";
        OutputStream output = null;
        active.add(entry.id);
        try {
            rocksDBService.putSync(PREFIX + entry.id, entry);
            // Retain this CREATE_NEW handle: reopening by path could truncate a replacement file.
            FileAttribute<?>[] permissions = Files.getFileStore(root).supportsFileAttributeView("posix")
                    ? new FileAttribute<?>[]{PosixFilePermissions.asFileAttribute(PosixFilePermissions.fromString("rw-------"))}
                    : new FileAttribute<?>[0];
            output = Channels.newOutputStream(Files.newByteChannel(Paths.get(entry.temp),
                    new HashSet<OpenOption>(Arrays.asList(StandardOpenOption.CREATE_NEW,
                            StandardOpenOption.WRITE, LinkOption.NOFOLLOW_LINKS)), permissions));
            BasicFileAttributes file = attributes(Paths.get(entry.temp));
            if (!file.isRegularFile() || file.fileKey() == null) {
                throw new IOException("temporary file has no stable identity");
            }
            entry.fileKey = file.fileKey().toString();
            entry.stage = "OPEN";
            rocksDBService.putSync(PREFIX + entry.id, entry);
            return new OwnedFile(entry, output);
        } catch (IOException | RuntimeException failure) {
            if (output != null) try { output.close(); } catch (IOException closeFailure) { failure.addSuppressed(closeFailure); }
            try { cleanup(entry); } catch (IOException | RuntimeException cleanupFailure) { failure.addSuppressed(cleanupFailure); }
            active.remove(entry.id);
            throw failure;
        }
    }

    public final class OwnedFile implements AutoCloseable {
        private final Entry entry;
        private final OutputStream output;
        private boolean closed;
        private OwnedFile(Entry entry, OutputStream output) { this.entry = entry; this.output = output; }
        public Path getPath() { return Paths.get(entry.temp); }
        public OutputStream getOutputStream() { return output; }
        public void verifyIdentity() throws IOException {
            synchronized (LocalTemporaryFileRegistry.this) {
                validatePaths(entry);
                if (!matches(entry, attributes(getPath()))) throw new IOException("temporary-file identity changed");
            }
        }
        @Override public void close() throws IOException {
            synchronized (LocalTemporaryFileRegistry.this) {
                if (closed) return;
                closed = true;
                IOException closeFailure = null;
                try { output.close(); } catch (IOException failure) { closeFailure = failure; }
                try { cleanup(entry); }
                catch (IOException | RuntimeException failure) {
                    if (closeFailure == null) {
                        if (failure instanceof IOException) closeFailure = (IOException) failure;
                        else throw (RuntimeException) failure;
                    } else closeFailure.addSuppressed(failure);
                } finally { active.remove(entry.id); }
                if (closeFailure != null) throw closeFailure;
            }
        }
    }

    private void cleanup(Entry entry) throws IOException {
        Path path = validatePaths(entry);
        BasicFileAttributes file;
        try { file = attributes(path); }
        catch (NoSuchFileException missing) {
            // Includes a completed atomic rename: never follow its inode to the final destination.
            forget(entry);
            return;
        }
        if (!"OPEN".equals(entry.stage) || !matches(entry, file)) {
            log.warn("保留身份无法确认的临时文件: {} ({})", entry.temp, entry.stage);
            return;
        }
        // Recheck immediately before unlink; never follow a symlink or search a directory by glob.
        if (!matches(entry, attributes(path))) throw new IOException("temporary-file identity changed before cleanup");
        Files.delete(path);
        forget(entry);
    }

    private void forget(Entry entry) {
        try { rocksDBService.delete(PREFIX + entry.id); }
        catch (RuntimeException failure) {
            // An unsynced/missed deletion is safe: next startup sees only the absent exact temp path.
            log.warn("临时文件已移除，保留待重试的登记: {}", entry.id, failure);
        }
    }

    private Path validatePaths(Entry entry) throws IOException {
        if (entry.root == null || entry.temp == null || entry.owner == null) throw new IOException("incomplete temporary-file ownership");
        Path root = Paths.get(entry.root), path = Paths.get(entry.temp);
        if (!root.isAbsolute() || !path.isAbsolute() || !root.equals(root.normalize())
                || !path.equals(path.normalize()) || !path.startsWith(root) || path.equals(root)) {
            throw new IOException("unsafe temporary-file ownership path");
        }
        BasicFileAttributes rootAttributes = attributes(root);
        if (!root.equals(root.toRealPath()) || !rootAttributes.isDirectory()
                || rootAttributes.fileKey() == null || !rootAttributes.fileKey().toString().equals(entry.rootFileKey)) {
            throw new IOException("temporary-file root identity changed");
        }
        String expectedName = ".transfer-" + (entry.taskId == null ? "unknown" : entry.taskId) + "-" + entry.id + ".part";
        if (!expectedName.equals(path.getFileName().toString())) throw new IOException("temporary-file name does not match ownership");
        if (entry.target != null) {
            Path target = Paths.get(entry.target);
            if (!target.isAbsolute() || !target.equals(target.normalize()) || !target.startsWith(root)
                    || target.equals(path) || !path.getParent().equals(target.getParent())) {
                throw new IOException("temporary-file destination does not match ownership");
            }
        }
        requireSafeChain(root, path.getParent());
        return path;
    }

    private static boolean matches(Entry entry, BasicFileAttributes file) {
        // Do not compare creationTime: JDK 8 file providers may substitute mutable mtime for it.
        return file.isRegularFile() && !file.isSymbolicLink() && file.fileKey() != null
                && file.fileKey().toString().equals(entry.fileKey);
    }

    private static BasicFileAttributes attributes(Path path) throws IOException {
        return Files.readAttributes(path, BasicFileAttributes.class, LinkOption.NOFOLLOW_LINKS);
    }

    private static void requireSafeChain(Path root, Path parent) throws IOException {
        if (parent == null || !parent.startsWith(root)) throw new IOException("temporary file escapes configured root");
        Path current = root;
        for (Path part : root.relativize(parent)) {
            current = current.resolve(part);
            BasicFileAttributes attributes = attributes(current);
            if (!attributes.isDirectory() || attributes.isSymbolicLink()) throw new IOException("unsafe temporary-file parent");
        }
    }
}
