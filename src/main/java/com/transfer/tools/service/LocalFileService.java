package com.transfer.tools.service;

import com.transfer.tools.model.ConflictPolicy;
import com.transfer.tools.model.LocalFileItem;
import org.springframework.stereotype.Service;

import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.nio.file.*;
import java.nio.file.attribute.BasicFileAttributes;
import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import java.util.function.Consumer;

@Service
public class LocalFileService {
    @org.springframework.beans.factory.annotation.Autowired(required = false)
    private LocalTaskControlService localTaskControlService;
    @org.springframework.beans.factory.annotation.Autowired
    private LocalTemporaryFileRegistry temporaryFileRegistry;

    public Path requireAbsolute(String rawPath) {
        if (rawPath == null || rawPath.indexOf('\0') >= 0) {
            throw new TransferValidationException("LOCAL_PATH_UNSAFE", "local path is empty or contains NUL");
        }
        Path path = Paths.get(rawPath);
        if (!path.isAbsolute()) {
            throw new TransferValidationException("LOCAL_PATH_NOT_ABSOLUTE", "local path must be absolute");
        }
        return path.normalize();
    }

    public void validateSource(String rawPath) {
        Path path = requireAbsolute(rawPath);
        if (!Files.exists(path, LinkOption.NOFOLLOW_LINKS)) {
            throw new TransferValidationException("LOCAL_PATH_NOT_FOUND", "local source does not exist");
        }
        if (Files.isSymbolicLink(path) || (!Files.isRegularFile(path, LinkOption.NOFOLLOW_LINKS)
                && !Files.isDirectory(path, LinkOption.NOFOLLOW_LINKS))) {
            throw new TransferValidationException("LOCAL_PATH_UNSAFE", "local source must be a regular file or directory and cannot be a symlink");
        }
        if (!Files.isReadable(path)) {
            throw new TransferValidationException("LOCAL_PATH_NOT_READABLE", "local source is not readable");
        }
    }

    public void validateDestination(String rawPath, boolean createLeaf) {
        Path path = requireAbsolute(rawPath);
        try {
            if (Files.isSymbolicLink(path)) {
                throw new TransferValidationException("LOCAL_PATH_UNSAFE", "local destination cannot be a symbolic link");
            }
            if (!Files.exists(path, LinkOption.NOFOLLOW_LINKS) && createLeaf) {
                Files.createDirectories(path);
            }
        } catch (IOException e) {
            throw new TransferValidationException("LOCAL_PATH_NOT_WRITABLE", "cannot create local destination: " + e.getMessage());
        }
        if (!Files.isDirectory(path, LinkOption.NOFOLLOW_LINKS) || !Files.isWritable(path)) {
            throw new TransferValidationException("LOCAL_PATH_NOT_WRITABLE", "local destination must be a writable directory");
        }
    }

    public InputStream openInput(Path file) throws IOException {
        if (Files.isSymbolicLink(file) || !Files.isRegularFile(file, LinkOption.NOFOLLOW_LINKS)) {
            throw new IOException("source is not a regular file");
        }
        return Files.newInputStream(file, StandardOpenOption.READ);
    }

    public Path resolveSource(String rootPath, String relativeKey) {
        Path root = requireAbsolute(rootPath);
        if (relativeKey == null || relativeKey.isEmpty() || relativeKey.indexOf('\0') >= 0) {
            throw new TransferValidationException("LOCAL_PATH_UNSAFE", "empty or unsafe source key");
        }
        try {
            root = root.toRealPath();
        } catch (IOException e) {
            throw new TransferValidationException("LOCAL_PATH_NOT_FOUND", "local source root does not exist");
        }
        if (Files.isRegularFile(root, LinkOption.NOFOLLOW_LINKS)) {
            // A shared single file may be mounted under a different absolute file name on each
            // Worker. The binding itself is the security boundary, so always return that file.
            return root;
        }
        Path result = root.resolve(relativeKey.replace('\\', '/')).normalize();
        if (!result.startsWith(root)) {
            throw new TransferValidationException("LOCAL_PATH_UNSAFE", "source escapes configured root");
        }
        rejectSymlinkChain(root, result);
        if (!Files.isRegularFile(result, LinkOption.NOFOLLOW_LINKS) || !Files.isReadable(result)) {
            throw new TransferValidationException("LOCAL_PATH_NOT_READABLE", "local source item is not a readable regular file");
        }
        return result;
    }

    public Path resolveDestination(String rootPath, String relativeKey) {
        Path configuredRoot = requireAbsolute(rootPath);
        if (relativeKey == null || relativeKey.isEmpty() || relativeKey.indexOf('\0') >= 0) {
            throw new TransferValidationException("LOCAL_PATH_UNSAFE", "empty or unsafe destination key");
        }
        String portableKey = relativeKey.replace('\\', '/');
        if (portableKey.startsWith("/") || Paths.get(portableKey).isAbsolute()) {
            throw new TransferValidationException("LOCAL_PATH_UNSAFE", "absolute object key is not allowed");
        }
        Path root;
        try {
            root = configuredRoot.toRealPath();
        } catch (IOException e) {
            throw new TransferValidationException("LOCAL_PATH_NOT_FOUND", "local destination root does not exist");
        }
        Path result = root.resolve(portableKey).normalize();
        if (!result.startsWith(root)) {
            throw new TransferValidationException("LOCAL_PATH_UNSAFE", "destination escapes configured root");
        }
        rejectSymlinkChain(root, result.getParent());
        return result;
    }

    public boolean writeAtomically(InputStream input, String rootPath, String relativeKey,
                                   Integer taskId, ConflictPolicy policy) throws IOException {
        return writeAtomically(input, rootPath, relativeKey, taskId, policy, -1);
    }

    public boolean writeAtomically(InputStream input, String rootPath, String relativeKey,
                                   Integer taskId, ConflictPolicy policy, long expectedSize) throws IOException {
        Path target = resolveDestination(rootPath, relativeKey);
        if (Files.exists(target, LinkOption.NOFOLLOW_LINKS)) {
            if (!Files.isRegularFile(target, LinkOption.NOFOLLOW_LINKS) || Files.isSymbolicLink(target)) {
                throw new IOException("destination collides with a directory or unsafe file");
            }
            if (policy != ConflictPolicy.OVERWRITE) {
                return false;
            }
        }
        Path parent = target.getParent();
        Files.createDirectories(parent);
        rejectSymlinkChain(requireAbsolute(rootPath).toRealPath(), parent);
        try (LocalTemporaryFileRegistry.OwnedFile temporary = temporaryFileRegistry.create(
                requireAbsolute(rootPath).toRealPath(), target, relativeKey, taskId)) {
            long written = 0;
            try (OutputStream output = temporary.getOutputStream()) {
                byte[] buffer = new byte[1024 * 1024];
                int read;
                while ((read = input.read(buffer)) >= 0) {
                    if (read > 0) {
                        output.write(buffer, 0, read);
                        written += read;
                    }
                }
            }
            if (expectedSize >= 0 && written != expectedSize) {
                throw new IOException("download length mismatch: expected " + expectedSize + ", received " + written);
            }
            if (localTaskControlService != null) localTaskControlService.checkRunning(taskId);
            temporary.verifyIdentity();
            CopyOption[] options = policy == ConflictPolicy.OVERWRITE
                    ? new CopyOption[]{StandardCopyOption.ATOMIC_MOVE, StandardCopyOption.REPLACE_EXISTING}
                    : new CopyOption[]{StandardCopyOption.ATOMIC_MOVE};
            Files.move(temporary.getPath(), target, options);
            return true;
        }
    }

    public void createDirectoryMarker(String rootPath, String relativeKey) throws IOException {
        String key = relativeKey;
        while (key.endsWith("/")) {
            key = key.substring(0, key.length() - 1);
        }
        if (key.isEmpty()) {
            return;
        }
        Path target = resolveDestination(rootPath, key);
        Files.createDirectories(target);
        rejectSymlinkChain(requireAbsolute(rootPath).toRealPath(), target);
    }

    public void scan(String rawPath, Consumer<LocalFileItem> consumer) throws IOException {
        Path source = requireAbsolute(rawPath);
        validateSource(rawPath);
        if (Files.isRegularFile(source, LinkOption.NOFOLLOW_LINKS)) {
            BasicFileAttributes attributes = Files.readAttributes(source, BasicFileAttributes.class, LinkOption.NOFOLLOW_LINKS);
            consumer.accept(new LocalFileItem(source, source.getFileName().toString(), attributes.size(), attributes.lastModifiedTime().toMillis()));
            return;
        }
        scanDirectory(source, source, consumer);
    }

    /**
     * Resume in the scanner's actual traversal order. Comparing relative path strings with the
     * marker is unsafe because a depth-first walk is not globally lexicographic (for example,
     * {@code a/child} can be visited before {@code a.txt}). If the marker disappeared between
     * runs, replay the full scan; completed-item records make that replay safe.
     */
    public void scanAfter(String rawPath, String lastCommittedRelativeKey,
                          Consumer<LocalFileItem> consumer) throws IOException {
        if (lastCommittedRelativeKey == null || lastCommittedRelativeKey.isEmpty()) {
            scan(rawPath, consumer);
            return;
        }
        final boolean[] markerFound = {false};
        scan(rawPath, item -> {
            if (markerFound[0]) {
                consumer.accept(item);
            } else if (lastCommittedRelativeKey.equals(item.getRelativeKey())) {
                markerFound[0] = true;
            }
        });
        if (!markerFound[0]) {
            scan(rawPath, consumer);
        }
    }

    private void scanDirectory(Path root, Path directory, Consumer<LocalFileItem> consumer) throws IOException {
        List<Path> children = new ArrayList<>();
        try (DirectoryStream<Path> stream = Files.newDirectoryStream(directory)) {
            for (Path child : stream) {
                children.add(child);
            }
        }
        Collections.sort(children, (a, b) -> a.getFileName().toString().compareTo(b.getFileName().toString()));
        for (Path child : children) {
            if (Files.isSymbolicLink(child)) {
                continue;
            }
            if (Files.isDirectory(child, LinkOption.NOFOLLOW_LINKS)) {
                scanDirectory(root, child, consumer);
            } else if (Files.isRegularFile(child, LinkOption.NOFOLLOW_LINKS)) {
                BasicFileAttributes attributes = Files.readAttributes(child, BasicFileAttributes.class, LinkOption.NOFOLLOW_LINKS);
                String key = root.relativize(child).toString().replace(FileSystems.getDefault().getSeparator(), "/");
                consumer.accept(new LocalFileItem(child, key, attributes.size(), attributes.lastModifiedTime().toMillis()));
            }
        }
    }

    private void rejectSymlinkChain(Path root, Path path) {
        if (root == null || path == null) {
            return;
        }
        Path absolute = path.toAbsolutePath().normalize();
        if (!absolute.startsWith(root)) {
            throw new TransferValidationException("LOCAL_PATH_UNSAFE", "destination escapes configured root");
        }
        Path current = root;
        for (Path part : root.relativize(absolute)) {
            current = current.resolve(part);
            if (Files.exists(current, LinkOption.NOFOLLOW_LINKS) && Files.isSymbolicLink(current)) {
                throw new TransferValidationException("LOCAL_PATH_UNSAFE", "symbolic links are not allowed in local path");
            }
        }
    }
}
