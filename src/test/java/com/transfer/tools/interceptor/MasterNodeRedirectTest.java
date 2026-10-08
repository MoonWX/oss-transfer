package com.transfer.tools.interceptor;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.config.DistributedConfig;
import org.junit.*;
import org.springframework.mock.web.MockHttpServletRequest;
import org.springframework.mock.web.MockHttpServletResponse;
import org.springframework.test.util.ReflectionTestUtils;
import static org.junit.Assert.*;

public class MasterNodeRedirectTest {
    private MasterNodeInterceptor interceptor;
    private DistributedConfig config;
    @Before public void setup() {
        config = new DistributedConfig(); config.setMasterIp("10.0.0.1"); config.setLocalIp("10.0.0.2"); config.setPort(1785);
        config.setMasterWebUrl("https://transfer.example.test");
        interceptor = new MasterNodeInterceptor(); ReflectionTestUtils.setField(interceptor, "distributedConfig", config);
        ReflectionTestUtils.setField(interceptor, "objectMapper", new ObjectMapper());
    }
    @Test public void deepLinkAndLoginRedirectToConfiguredOriginWithoutUsingHostOrReturnUrl() throws Exception {
        MockHttpServletRequest request = new MockHttpServletRequest("GET", "/tasks");
        request.addHeader("Host", "evil.example"); request.addHeader("X-Forwarded-Host", "evil.example");
        request.setQueryString("taskId=100&returnUrl=https%3A%2F%2Fevil.example");
        MockHttpServletResponse response = new MockHttpServletResponse();
        assertFalse(interceptor.preHandle(request, response, new Object()));
        assertEquals(302, response.getStatus());
        assertEquals("https://transfer.example.test/tasks?taskId=100&returnUrl=https%3A%2F%2Fevil.example", response.getHeader("Location"));
        response = new MockHttpServletResponse();
        assertFalse(interceptor.preHandle(new MockHttpServletRequest("GET", "/login.html"), response, new Object()));
        assertEquals("https://transfer.example.test/login.html", response.getHeader("Location"));
    }
    @Test public void workerApisAssetsHealthAndAuthRequestsRemainLocal() throws Exception {
        for (String path : new String[]{"/api/worker/status", "/api/node-registration/proof", "/actuator/health", "/health", "/assets/index.js", "/favicon.ico"}) {
            MockHttpServletResponse response = new MockHttpServletResponse();
            assertTrue(path, interceptor.preHandle(new MockHttpServletRequest("GET", path), response, new Object()));
            assertNull(response.getHeader("Location"));
        }
        assertTrue(interceptor.preHandle(new MockHttpServletRequest("POST", "/api/auth/login"), new MockHttpServletResponse(), new Object()));
    }
    @Test public void masterApiStillReturnsForbiddenOnWorkerAndMasterPagesStayLocal() throws Exception {
        MockHttpServletResponse response = new MockHttpServletResponse();
        assertFalse(interceptor.preHandle(new MockHttpServletRequest("GET", "/api/master/status"), response, new Object()));
        assertEquals(403, response.getStatus()); assertNull(response.getHeader("Location"));
        config.setLocalIp("10.0.0.1");
        assertTrue(interceptor.preHandle(new MockHttpServletRequest("GET", "/tasks"), new MockHttpServletResponse(), new Object()));
    }
    @Test public void malformedOriginsNeverBecomeOpenRedirectsAndMissingSettingUsesConfiguredMaster() throws Exception {
        for (String value : new String[]{"//evil.test", "javascript:alert(1)", "https://user:pass@master.test", "https://master.test/?next=evil", "https://master.test/path", "http://master.test:0", "https://master.test:65536"}) {
            config.setMasterWebUrl(value); MockHttpServletResponse response = new MockHttpServletResponse();
            assertFalse(interceptor.preHandle(new MockHttpServletRequest("GET", "/"), response, new Object()));
            assertEquals(503, response.getStatus()); assertNull(response.getHeader("Location"));
        }
        config.setMasterWebUrl(null); assertEquals("http://10.0.0.1:1785", config.getMasterWebUrl());
    }
    @Test public void fallbackRedirectAndMasterApiMetadataUseTheCustomServicePort() throws Exception {
        config.setMasterWebUrl(null);
        config.setPort(18443);
        MockHttpServletRequest page = new MockHttpServletRequest("GET", "/tasks");
        page.setQueryString("taskId=224");
        MockHttpServletResponse response = new MockHttpServletResponse();
        assertFalse(interceptor.preHandle(page, response, new Object()));
        assertEquals("http://10.0.0.1:18443/tasks?taskId=224", response.getHeader("Location"));
        response = new MockHttpServletResponse();
        assertFalse(interceptor.preHandle(new MockHttpServletRequest("GET", "/api/master/status"), response, new Object()));
        assertEquals(403, response.getStatus());
        assertEquals(18443, new ObjectMapper().readTree(response.getContentAsString()).get("masterPort").asInt());
    }

}
