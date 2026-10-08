package com.transfer.tools.config;

import com.transfer.tools.interceptor.AuthInterceptor;
import com.transfer.tools.interceptor.MasterNodeInterceptor;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.context.annotation.Configuration;
import org.springframework.web.servlet.config.annotation.InterceptorRegistry;
import org.springframework.web.servlet.config.annotation.WebMvcConfigurer;

@Configuration
public class WebMvcConfig implements WebMvcConfigurer {

    @Autowired
    private MasterNodeInterceptor masterNodeInterceptor;

    @Autowired
    private AuthInterceptor authInterceptor;

    @Override
    public void addInterceptors(InterceptorRegistry registry) {
        // API master-role checks plus worker document redirects. The interceptor excludes assets/health.
        registry.addInterceptor(masterNodeInterceptor)
                .addPathPatterns("/**");

        // 登录认证：拦截所有 API 接口（AuthInterceptor 内部会放行 /api/auth/）
        registry.addInterceptor(authInterceptor)
                .addPathPatterns("/api/**");
    }
}
