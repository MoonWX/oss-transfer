import org.rocksdb.*;
import java.nio.file.*;
import java.nio.charset.StandardCharsets;
import java.util.Base64;

/** Test-only writer; never used by the exporter. */
public final class WriteFixture {
    public static void main(String[] args) throws Exception {
        RocksDB.loadLibrary();
        try (Options options = new Options().setCreateIfMissing(true).setAvoidFlushDuringShutdown(true);
             RocksDB db = RocksDB.open(options, args[0]); WriteOptions write = new WriteOptions().setSync(true)) {
            for (String line : Files.readAllLines(Paths.get(args[1]), StandardCharsets.UTF_8)) {
                String[] parts = line.split("\\t", -1);
                db.put(write, Base64.getUrlDecoder().decode(parts[0]), Base64.getUrlDecoder().decode(parts[1]));
            }
            System.out.println("READY"); System.out.flush();
            if (args.length > 2) System.in.read();
        }
    }
}
