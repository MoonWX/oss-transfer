import java.io.*;
import java.lang.reflect.*;
import java.net.*;
import java.nio.channels.*;
import java.nio.charset.StandardCharsets;
import java.nio.file.*;
import java.nio.file.attribute.*;
import java.security.*;
import java.time.Instant;
import java.util.*;
import java.util.jar.*;

/** Standalone, read-only RocksDB export. No application classes are loaded. */
public final class LegacyExport {
    private static final long MAX_VALUE = 64L << 20;
    private static final Base64.Encoder B64 = Base64.getUrlEncoder().withoutPadding();
    private static final FileAttribute<Set<PosixFilePermission>> PRIVATE_FILE =
            PosixFilePermissions.asFileAttribute(PosixFilePermissions.fromString("rw-------"));
    private static final FileAttribute<Set<PosixFilePermission>> PRIVATE_DIR =
            PosixFilePermissions.asFileAttribute(PosixFilePermissions.fromString("rwx------"));

    public static void main(String[] args) {
        try { run(args); }
        catch (Throwable failure) {
            // JNI errors can contain key/value data; do not print arbitrary exception text.
            String message = failure instanceof ExportFailure ? failure.getMessage() : failure.getClass().getSimpleName();
            System.err.println("Legacy export failed: " + message);
            System.exit(1);
        }
    }

    private static void run(String[] args) throws Exception {
        Map<String, String> options = new HashMap<>();
        Set<String> names = new HashSet<>(Arrays.asList("--legacy-jar", "--expected-jar-sha256", "--database", "--output", "--node", "--role", "--config"));
        for (int i = 0; i < args.length; i += 2) {
            if (i + 1 >= args.length || !names.contains(args[i]) || options.put(args[i], args[i + 1]) != null)
                throw bad("invalid or repeated option");
        }
        for (String name : names) if (!name.equals("--config") && !options.containsKey(name)) throw bad("missing " + name);
        String node = options.get("--node"), role = options.get("--role");
        if (node.isEmpty() || node.length() > 256 || node.matches("(?s).*\\p{Cntrl}.*")) throw bad("invalid node identifier");
        if (!Arrays.asList("master", "agent", "worker").contains(role)) throw bad("role must be master, agent or worker");
        Path jar = regular(options.get("--legacy-jar"));
        String expected = options.get("--expected-jar-sha256");
        if (!expected.matches("[a-f0-9]{64}") || !expected.equals(hash(jar))) throw bad("legacy JAR checksum mismatch");
        Path db = Paths.get(options.get("--database")).toAbsolutePath().normalize();
        if (Files.isSymbolicLink(db) || !Files.isDirectory(db, LinkOption.NOFOLLOW_LINKS)
                || !Files.isRegularFile(db.resolve("CURRENT"), LinkOption.NOFOLLOW_LINKS)
                || !Files.isRegularFile(db.resolve("LOCK"), LinkOption.NOFOLLOW_LINKS)) throw bad("not an existing RocksDB directory");
        Path output = Paths.get(options.get("--output")).toAbsolutePath().normalize();
        Path parent = output.getParent().toRealPath();
        if (!Files.getFileStore(parent).supportsFileAttributeView("posix")) throw bad("export requires POSIX private file permissions");
        output = parent.resolve(output.getFileName());
        if (output.startsWith(db.toRealPath())) throw bad("output must be outside the source database");
        if (Files.exists(output, LinkOption.NOFOLLOW_LINKS)) throw bad("output already exists");
        byte[] config = options.containsKey("--config") ? readBounded(regular(options.get("--config")), 4 << 20) : null;
        Path scratch = Files.createTempDirectory(parent, ".legacy-export-", PRIVATE_DIR);
        Path partial = scratch.resolve("snapshot.partial");
        boolean published = false;
        try {
            Path library = extractLibrary(jar, scratch);
            try (FileChannel lockFile = FileChannel.open(db.resolve("LOCK"), StandardOpenOption.READ)) {
                FileLock lock;
                try { lock = lockFile.tryLock(0, Long.MAX_VALUE, true); }
                catch (OverlappingFileLockException e) { throw bad("database is in use; stop the old service first"); }
                if (lock == null) throw bad("database is in use; stop the old service first");
                try (FileLock held = lock) {
                    Map<String, String> before = databaseIdentity(db);
                    try (URLClassLoader loader = new URLClassLoader(new URL[]{library.toUri().toURL()}, ClassLoader.getSystemClassLoader().getParent())) {
                        Class<?> databaseClass = loader.loadClass("org.rocksdb.RocksDB");
                        databaseClass.getMethod("loadLibrary").invoke(null);
                        Class<?> optionsClass = loader.loadClass("org.rocksdb.Options");
                        Object rocksOptions = optionsClass.getConstructor().newInstance();
                        try {
                            call(rocksOptions, "setCreateIfMissing", new Class<?>[]{boolean.class}, false);
                            call(rocksOptions, "setDbLogDir", new Class<?>[]{String.class}, scratch.toString());
                            Object database = databaseClass.getMethod("openReadOnly", optionsClass, String.class).invoke(null, rocksOptions, db.toString());
                            try {
                                Files.createFile(partial, PRIVATE_FILE);
                                export(database, partial, node, role, expected, hash(library), config);
                            } finally { call(database, "close"); }
                        } finally { call(rocksOptions, "close"); }
                    }
                    if (!before.equals(databaseIdentity(db))) throw bad("database changed during export; no snapshot published");
                }
            }
            // createLink fails atomically if the destination exists. Never replace an old export.
            Files.createLink(output, partial);
            published = true;
            try (FileChannel dir = FileChannel.open(parent, StandardOpenOption.READ)) { dir.force(true); }
            System.out.println("Exported: " + output);
        } finally {
            // Only files inside this process's private scratch directory are removed.
            try (java.util.stream.Stream<Path> paths = Files.walk(scratch)) {
                Iterator<Path> iterator = paths.sorted(Comparator.reverseOrder()).iterator();
                while (iterator.hasNext()) Files.deleteIfExists(iterator.next());
            }
            if (!published) System.err.println("No complete snapshot was published.");
        }
    }

    private static void export(Object database, Path output, String node, String role, String jarHash, String jniHash, byte[] config) throws Exception {
        MessageDigest digest = MessageDigest.getInstance("SHA-256");
        try (FileOutputStream raw = new FileOutputStream(output.toFile()); BufferedOutputStream out = new BufferedOutputStream(raw)) {
            line(out, digest, "{\"type\":\"header\",\"schemaVersion\":1,\"node\":" + quote(node)
                    + ",\"role\":" + quote(role) + ",\"legacyJarSha256\":" + quote(jarHash)
                    + ",\"rocksdbJniSha256\":" + quote(jniHash) + ",\"exportedAt\":" + quote(Instant.now().toString()) + "}");
            if (config != null) line(out, digest, "{\"type\":\"configuration\",\"name\":\"application.yml\",\"value\":\"" + B64.encodeToString(config) + "\"}");
            Object iterator = call(database, "newIterator");
            long count = 0, keyBytes = 0, valueBytes = 0;
            try {
                call(iterator, "seekToFirst");
                while ((Boolean) call(iterator, "isValid")) {
                    byte[] key = (byte[]) call(iterator, "key"), value = (byte[]) call(iterator, "value");
                    if (key.length > (1 << 20) || value.length > MAX_VALUE) throw bad("entry exceeds the export size bound");
                    line(out, digest, "{\"type\":\"entry\",\"key\":\"" + B64.encodeToString(key) + "\",\"value\":\"" + B64.encodeToString(value) + "\"}");
                    count = Math.addExact(count, 1); keyBytes = Math.addExact(keyBytes, key.length); valueBytes = Math.addExact(valueBytes, value.length);
                    call(iterator, "next");
                }
                call(iterator, "status");
            } finally { call(iterator, "close"); }
            line(out, null, "{\"type\":\"footer\",\"entries\":" + count + ",\"keyBytes\":" + keyBytes + ",\"valueBytes\":" + valueBytes
                    + ",\"sha256\":" + quote(hex(digest.digest())) + "}");
            out.flush(); raw.getFD().sync();
        }
    }

    private static Object call(Object value, String name) throws Exception { return call(value, name, new Class<?>[0]); }
    private static Object call(Object value, String name, Class<?>[] types, Object... args) throws Exception {
        return value.getClass().getMethod(name, types).invoke(value, args);
    }
    private static void line(OutputStream out, MessageDigest digest, String value) throws IOException {
        byte[] bytes = (value + "\n").getBytes(StandardCharsets.UTF_8); out.write(bytes); if (digest != null) digest.update(bytes);
    }
    private static String quote(String value) {
        StringBuilder b = new StringBuilder("\"");
        for (int i = 0; i < value.length(); i++) {
            char c = value.charAt(i);
            if (c == '"' || c == '\\') b.append('\\').append(c);
            else if (c < 32) b.append(String.format("\\u%04x", (int)c));
            else b.append(c);
        }
        return b.append('"').toString();
    }
    private static Path regular(String value) throws IOException {
        Path p = Paths.get(value).toAbsolutePath().normalize();
        if (!Files.isRegularFile(p, LinkOption.NOFOLLOW_LINKS)) throw bad("expected a regular file"); return p;
    }
    private static byte[] readBounded(Path file, long maximum) throws IOException {
        if (Files.size(file) > maximum) throw bad("configuration exceeds the size bound"); return Files.readAllBytes(file);
    }
    private static Path extractLibrary(Path jar, Path scratch) throws Exception {
        Path target = scratch.resolve("rocksdbjni.jar");
        try (JarFile archive = new JarFile(jar.toFile())) {
            JarEntry match = null;
            Enumeration<JarEntry> entries = archive.entries();
            while (entries.hasMoreElements()) {
                JarEntry entry = entries.nextElement();
                if (entry.getName().matches("BOOT-INF/lib/rocksdbjni-[0-9.]+\\.jar")) {
                    if (match != null) throw bad("multiple RocksDB libraries in legacy JAR"); match = entry;
                }
            }
            if (match == null || match.getSize() > (256L << 20)) throw bad("legacy JAR has no supported RocksDB library");
            Files.createFile(target, PRIVATE_FILE);
            try (InputStream in = archive.getInputStream(match); OutputStream out = Files.newOutputStream(target)) {
                byte[] buffer = new byte[65536]; long count = 0; int n;
                while ((n = in.read(buffer)) != -1) { count += n; if (count > (256L << 20)) throw bad("RocksDB library exceeds the size bound"); out.write(buffer, 0, n); }
            }
        }
        return target;
    }
    private static Map<String, String> databaseIdentity(Path db) throws Exception {
        Map<String, String> result = new TreeMap<>();
        try (DirectoryStream<Path> files = Files.newDirectoryStream(db)) {
            for (Path path : files) {
                BasicFileAttributes a = Files.readAttributes(path, BasicFileAttributes.class, LinkOption.NOFOLLOW_LINKS);
                if (!a.isRegularFile()) throw bad("database directory contains a non-regular entry");
                result.put(path.getFileName().toString(), a.size() + ":" + a.lastModifiedTime() + ":" + a.fileKey());
            }
        }
        return result;
    }
    private static String hash(Path file) throws Exception {
        MessageDigest d = MessageDigest.getInstance("SHA-256");
        try (InputStream in = Files.newInputStream(file)) { byte[] buffer = new byte[65536]; int n; while ((n = in.read(buffer)) != -1) d.update(buffer, 0, n); }
        return hex(d.digest());
    }
    private static String hex(byte[] value) { StringBuilder b = new StringBuilder(); for (byte v : value) b.append(String.format("%02x", v & 255)); return b.toString(); }
    private static ExportFailure bad(String text) { return new ExportFailure(text); }
    private static final class ExportFailure extends IOException { ExportFailure(String message) { super(message); } }
}
