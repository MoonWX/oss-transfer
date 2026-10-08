package com.transfer.tools.service;

import com.transfer.tools.storage.RocksDBService;
import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;
import org.springframework.test.util.ReflectionTestUtils;

import static org.junit.Assert.*;

public class UserServiceTest {
    @Rule public TemporaryFolder temporary = new TemporaryFolder();
    private RocksDBService database;

    @Before public void open() throws Exception {
        database = new RocksDBService();
        ReflectionTestUtils.setField(database, "dbPath", temporary.newFolder("users").getAbsolutePath());
        database.init();
    }

    @After public void close() { if (database != null) database.destroy(); }

    @Test public void bootstrapPasswordCannotOverwritePasswordChangedInConsoleOnRestart() {
        UserService first = service("initial-password", false);
        first.init();
        assertNotNull(first.login("admin", "initial-password"));
        assertTrue(first.changePassword("admin", "initial-password", "changed-password"));
        String savedHash = first.getUser("admin").getPasswordHash();

        UserService restarted = service("initial-password", false);
        restarted.init();
        assertEquals(savedHash, restarted.getUser("admin").getPasswordHash());
        assertNull(restarted.login("admin", "initial-password"));
        assertNotNull(restarted.login("admin", "changed-password"));
    }

    @Test public void configuredHashIsOnlyBootstrapUnlessResetExplicitlyEnabled() {
        UserService first = service("initial-password", false);
        first.init();
        String bootstrapHash = first.getUser("admin").getPasswordHash();
        assertTrue(first.changePassword("admin", "initial-password", "changed-password"));

        UserService restarted = service(bootstrapHash, false);
        restarted.init();
        assertNotNull(restarted.login("admin", "changed-password"));
        assertNull(restarted.login("admin", "initial-password"));

        UserService reset = service(bootstrapHash, true);
        reset.init();
        assertEquals(bootstrapHash, reset.getUser("admin").getPasswordHash());
        assertNotNull(reset.login("admin", "initial-password"));
        assertNull(reset.login("admin", "changed-password"));
    }

    @Test public void explicitPlaintextResetWorksAndEmptyResetDoesNotReplacePassword() {
        UserService first = service("initial-password", false);
        first.init();
        UserService reset = service("recovery-password", true);
        reset.init();
        assertNull(reset.login("admin", "initial-password"));
        assertNotNull(reset.login("admin", "recovery-password"));
        String savedHash = reset.getUser("admin").getPasswordHash();

        UserService emptyReset = service("", true);
        emptyReset.init();
        assertEquals(savedHash, emptyReset.getUser("admin").getPasswordHash());
        assertNotNull(emptyReset.login("admin", "recovery-password"));
    }

    @Test public void logoutAndServiceRestartInvalidateSessionsWithoutChangingPassword() {
        UserService first = service("initial-password", false);
        first.init();
        String token = first.login("admin", "initial-password");
        assertEquals("admin", first.getUsernameByToken(token));
        first.logout(token);
        assertNull(first.getUsernameByToken(token));
        token = first.login("admin", "initial-password");
        UserService restarted = service("initial-password", false);
        restarted.init();
        assertNull(restarted.getUsernameByToken(token));
        assertNotNull(restarted.login("admin", "initial-password"));
        assertNull(restarted.getUserInfo("admin").getPasswordHash());
        assertNotNull(restarted.getUser("admin").getPasswordHash());
    }

    private UserService service(String password, boolean reset) {
        UserService users = new UserService();
        ReflectionTestUtils.setField(users, "rocksDBService", database);
        ReflectionTestUtils.setField(users, "defaultUsername", "admin");
        ReflectionTestUtils.setField(users, "defaultPassword", password);
        ReflectionTestUtils.setField(users, "resetPasswordOnStartup", reset);
        ReflectionTestUtils.setField(users, "tokenExpire", 86400000L);
        return users;
    }
}
