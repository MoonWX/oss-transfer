package com.transfer.tools.config;

import org.junit.Test;
import org.springframework.boot.env.YamlPropertySourceLoader;
import org.springframework.core.env.PropertySource;
import org.springframework.core.io.ClassPathResource;
import org.springframework.mock.env.MockEnvironment;
import org.springframework.test.util.ReflectionTestUtils;

import static org.junit.Assert.*;

public class DistributedConfigPortTest {
    private MockEnvironment defaults() throws Exception {
        MockEnvironment environment = new MockEnvironment();
        for (PropertySource<?> source : new YamlPropertySourceLoader().load("application", new ClassPathResource("application.yml"))) {
            environment.getPropertySources().addLast(source);
        }
        return environment;
    }

    private DistributedConfig config(MockEnvironment environment) {
        DistributedConfig config = new DistributedConfig();
        ReflectionTestUtils.setField(config, "environment", environment);
        config.init();
        return config;
    }

    private void assertPort(MockEnvironment environment, int expected) {
        assertEquals(Integer.valueOf(expected), environment.getProperty("server.port", Integer.class));
        assertEquals(Integer.valueOf(expected), config(environment).getPort());
    }

    @Test public void defaultAndEitherSinglePropertyOverrideUseTheActualListener() throws Exception {
        assertPort(defaults(), 1685);
        assertPort(defaults().withProperty("server.port", "1785"), 1785);
        assertPort(defaults().withProperty("transfer.port", "18443"), 18443);
        assertPort(defaults().withProperty("OSS_TRANSFER_PORT", "18888"), 18888);
    }

    @Test public void explicitListenerWinsOverAnOlderTransferDefault() throws Exception {
        MockEnvironment environment = defaults().withProperty("server.port", "1785").withProperty("transfer.port", "1685");
        assertPort(environment, 1785);
        DistributedConfig config = config(environment);
        config.setMasterIp("10.0.0.10");
        assertEquals("http://10.0.0.10:1785", config.getMasterWebUrl());
        config.setMasterWebUrl("https://public.example.test:18443");
        assertEquals("https://public.example.test:18443", config.getMasterWebUrl());
    }

    @Test public void standaloneLegacyTransferPropertyStillHasADefault() {
        assertEquals(Integer.valueOf(18888), config(new MockEnvironment().withProperty("transfer.port", "18888")).getPort());
        assertEquals(Integer.valueOf(1685), config(new MockEnvironment()).getPort());
    }

    @Test public void invalidEffectivePortsAreRejectedBeforeInternalCalls() throws Exception {
        for (String value : new String[]{"0", "-1", "65536", "2147483648", "invalid"}) {
            for (String field : new String[]{"server.port", "transfer.port"}) {
                try {
                    config(defaults().withProperty(field, value));
                    fail("accepted invalid " + field + ": " + value);
                } catch (RuntimeException expected) {
                    assertNotNull(expected.getMessage());
                }
            }
        }
        assertPort(defaults().withProperty("transfer.port", "1"), 1);
        assertPort(defaults().withProperty("server.port", "65535"), 65535);
    }
}
