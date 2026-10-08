package com.transfer.tools.controller;

import com.transfer.tools.config.ConsoleNotFoundViewResolver;
import org.junit.Test;
import org.junit.runner.RunWith;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.autoconfigure.EnableAutoConfiguration;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.boot.test.web.client.TestRestTemplate;
import org.springframework.context.annotation.Configuration;
import org.springframework.context.annotation.Import;
import org.springframework.http.*;
import org.springframework.test.context.junit4.SpringRunner;

import static org.junit.Assert.*;

/** Real servlet error dispatch: MockMvc alone does not dispatch container 404s to /error. */
@RunWith(SpringRunner.class)
@SpringBootTest(classes = ConsoleNotFoundNavigationTest.TestApplication.class,
        webEnvironment = SpringBootTest.WebEnvironment.RANDOM_PORT,
        properties = {"server.address=127.0.0.1", "spring.main.banner-mode=off",
                "logging.path=${java.io.tmpdir}/oss-v4-console-not-found-test"})
public class ConsoleNotFoundNavigationTest {
    @Autowired private TestRestTemplate http;

    @Test
    public void unknownBrowserRoutesServeConsoleEntryWithNotFoundStatus() {
        for (String path : new String[]{"/qa-missing-route", "/tasks/unknown", "/nested/missing/?taskId=42"}) {
            ResponseEntity<String> response = request(path, HttpMethod.GET, "text/html,application/xhtml+xml;q=0.9,*/*;q=0.8");
            assertEquals(path, HttpStatus.NOT_FOUND, response.getStatusCode());
            assertTrue(path, response.getBody().contains("<div id=\"root\"></div>"));
            assertFalse(path, response.getBody().contains("Whitelabel Error Page"));
        }
    }

    @Test
    public void missingApiEndpointsRemainJsonAndNeverReturnConsoleShell() {
        ResponseEntity<String> response = request("/api/unknown", HttpMethod.GET, "application/json");
        assertEquals(HttpStatus.NOT_FOUND, response.getStatusCode());
        assertTrue(MediaType.APPLICATION_JSON.isCompatibleWith(response.getHeaders().getContentType()));
        assertFalse(response.getBody().contains("<html"));
        // Even an explicit HTML request to a reserved root must not render the app shell.
        assertNoConsoleShell("/api/unknown", HttpMethod.GET, "text/html", HttpStatus.NOT_FOUND);
        assertNoConsoleShell("/api", HttpMethod.GET, "text/html", HttpStatus.NOT_FOUND);
    }

    @Test
    public void assetFailuresAndNonHtmlRequestsDoNotBecomeConsoleDocuments() {
        for (String path : new String[]{"/assets/missing.js", "/assets/extensionless", "/missing.css",
                "/favicon-missing.ico", "/webjars/missing"}) {
            assertNoConsoleShell(path, HttpMethod.GET, "text/html", HttpStatus.NOT_FOUND);
        }
        for (String accept : new String[]{"application/json", "*/*", "text/html;q=0,application/json"}) {
            assertNoConsoleShell("/qa-missing-route", HttpMethod.GET, accept, HttpStatus.NOT_FOUND);
        }
    }

    @Test
    public void knownConsolePagesAndRealStaticResourcesStillWork() {
        ResponseEntity<String> page = request("/tasks?taskId=42", HttpMethod.GET, "text/html");
        assertEquals(HttpStatus.OK, page.getStatusCode());
        assertTrue(page.getBody().contains("<div id=\"root\"></div>"));
        ResponseEntity<String> icon = request("/favicon.ico", HttpMethod.GET, "image/x-icon");
        assertEquals(HttpStatus.OK, icon.getStatusCode());
        assertFalse(icon.getBody().contains("<div id=\"root\"></div>"));
    }

    @Test
    public void missingWritesAndHeadRequestsAreNotForwardedToTheConsole() {
        assertNoConsoleShell("/qa-missing-route", HttpMethod.POST, "text/html", HttpStatus.NOT_FOUND);
        assertNoConsoleShell("/tasks", HttpMethod.POST, "text/html", HttpStatus.METHOD_NOT_ALLOWED);
        ResponseEntity<String> response = request("/qa-missing-route", HttpMethod.HEAD, "text/html");
        assertEquals(HttpStatus.NOT_FOUND, response.getStatusCode());
        assertNull(response.getBody());
    }

    private void assertNoConsoleShell(String path, HttpMethod method, String accept, HttpStatus status) {
        ResponseEntity<String> response = request(path, method, accept);
        assertEquals(path, status, response.getStatusCode());
        assertFalse(path, response.getBody() != null && response.getBody().contains("<div id=\"root\"></div>"));
    }

    private ResponseEntity<String> request(String path, HttpMethod method, String accept) {
        HttpHeaders headers = new HttpHeaders();
        headers.set(HttpHeaders.ACCEPT, accept);
        return http.exchange(path, method, new HttpEntity<Void>(headers), String.class);
    }

    @Configuration
    @EnableAutoConfiguration
    @Import({ConsolePageController.class, ConsoleNotFoundViewResolver.class})
    static class TestApplication { }
}
