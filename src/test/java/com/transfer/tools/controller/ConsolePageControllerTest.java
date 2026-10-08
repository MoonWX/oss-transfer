package com.transfer.tools.controller;

import org.junit.After;
import org.junit.Before;
import org.junit.Test;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Configuration;
import org.springframework.mock.web.MockServletContext;
import org.springframework.test.web.servlet.MockMvc;
import org.springframework.test.web.servlet.setup.MockMvcBuilders;
import org.springframework.web.context.support.AnnotationConfigWebApplicationContext;
import org.springframework.web.servlet.config.annotation.EnableWebMvc;
import org.springframework.web.servlet.config.annotation.ResourceHandlerRegistry;
import org.springframework.web.servlet.config.annotation.WebMvcConfigurer;

import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.get;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.post;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.forwardedUrl;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.status;

public class ConsolePageControllerTest {

    private AnnotationConfigWebApplicationContext context;
    private MockMvc mvc;

    @Before
    public void setUp() {
        context = new AnnotationConfigWebApplicationContext();
        context.setServletContext(new MockServletContext());
        context.register(TestConfiguration.class);
        context.refresh();
        mvc = MockMvcBuilders.webAppContextSetup(context).build();
    }

    @After
    public void tearDown() {
        context.close();
    }

    @Test
    public void directNavigationToEveryConsolePageForwardsToEntryPoint() throws Exception {
        for (String path : new String[]{"/", "/tasks", "/create", "/storage", "/nodes", "/nodes/adjustments", "/exceptions", "/tools"}) {
            mvc.perform(get(path))
                    .andExpect(status().isOk())
                    .andExpect(forwardedUrl("/index.html"));
        }
    }

    @Test
    public void pageQueryParametersDoNotPreventDeepLinkNavigation() throws Exception {
        mvc.perform(get("/tasks").param("status", "FAILED").param("task", "42"))
                .andExpect(status().isOk())
                .andExpect(forwardedUrl("/index.html"));
    }

    @Test
    public void missingApiEndpointsRemainNotFound() throws Exception {
        mvc.perform(get("/api/not-a-real-endpoint"))
                .andExpect(status().isNotFound())
                .andExpect(forwardedUrl(null));
    }

    @Test
    public void missingAssetsAndUnknownPagesRemainNotFound() throws Exception {
        for (String path : new String[]{"/assets/missing.js", "/assets/missing.css", "/unknown-page", "/tasks/unknown"}) {
            mvc.perform(get(path))
                    .andExpect(status().isNotFound())
                    .andExpect(forwardedUrl(null));
        }
    }

    @Test
    public void consolePageMappingDoesNotAcceptWrites() throws Exception {
        mvc.perform(post("/tasks"))
                .andExpect(status().isMethodNotAllowed())
                .andExpect(forwardedUrl(null));
    }

    @Configuration
    @EnableWebMvc
    static class TestConfiguration implements WebMvcConfigurer {

        @Bean
        public ConsolePageController consolePageController() {
            return new ConsolePageController();
        }

        @Override
        public void addResourceHandlers(ResourceHandlerRegistry registry) {
            registry.addResourceHandler("/**").addResourceLocations("classpath:/static/");
        }
    }
}
