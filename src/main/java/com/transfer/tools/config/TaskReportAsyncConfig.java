package com.transfer.tools.config;

import org.springframework.context.annotation.Configuration;
import org.springframework.web.context.request.NativeWebRequest;
import org.springframework.web.context.request.async.CallableProcessingInterceptor;
import org.springframework.web.servlet.config.annotation.AsyncSupportConfigurer;
import org.springframework.web.servlet.config.annotation.WebMvcConfigurer;

import javax.servlet.http.HttpServletRequest;
import java.util.concurrent.Callable;

/** Large report streams may outlive the servlet container's default async timeout. */
@Configuration
public class TaskReportAsyncConfig implements WebMvcConfigurer {
    static final long REPORT_TIMEOUT_MS = 60 * 60 * 1000L;

    @Override public void configureAsyncSupport(AsyncSupportConfigurer configurer) {
        configurer.registerCallableInterceptors(new CallableProcessingInterceptor() {
            @Override public <T> void preProcess(NativeWebRequest webRequest, Callable<T> task) {
                HttpServletRequest request = webRequest.getNativeRequest(HttpServletRequest.class);
                if (request != null && isReportDownload(request) && request.isAsyncStarted()) {
                    request.getAsyncContext().setTimeout(REPORT_TIMEOUT_MS);
                }
            }
        });
    }

    static boolean isReportDownload(HttpServletRequest request) {
        String uri = request.getRequestURI();
        return ("GET".equals(request.getMethod()) && uri.matches("/api/master/task-reports/[0-9]+"))
                || ("POST".equals(request.getMethod()) && uri.matches("/api/master/task-reports/[0-9]+/download"));
    }
}
