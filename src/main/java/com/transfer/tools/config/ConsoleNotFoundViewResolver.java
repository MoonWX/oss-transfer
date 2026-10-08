package com.transfer.tools.config;

import org.springframework.boot.autoconfigure.web.servlet.error.ErrorViewResolver;
import org.springframework.core.Ordered;
import org.springframework.http.HttpHeaders;
import org.springframework.http.HttpStatus;
import org.springframework.http.MediaType;
import org.springframework.stereotype.Component;
import org.springframework.web.servlet.ModelAndView;

import javax.servlet.RequestDispatcher;
import javax.servlet.http.HttpServletRequest;
import java.util.Map;

/** Lets browser navigation use the console's not-found view without masking missing APIs or assets. */
@Component
public class ConsoleNotFoundViewResolver implements ErrorViewResolver, Ordered {
    @Override
    public ModelAndView resolveErrorView(HttpServletRequest request, HttpStatus status, Map<String, Object> model) {
        if (status != HttpStatus.NOT_FOUND || !"GET".equals(request.getMethod()) || !acceptsHtml(request)) return null;
        Object originalUri = request.getAttribute(RequestDispatcher.ERROR_REQUEST_URI);
        if (!(originalUri instanceof String)) return null;
        String path = (String) originalUri;
        String contextPath = request.getContextPath();
        if (!contextPath.isEmpty() && path.startsWith(contextPath + "/")) path = path.substring(contextPath.length());
        // Extensions and reserved roots must retain their own HTTP error response.
        // Reject escaped/parameterized paths too, rather than interpreting them as routes.
        if (!path.startsWith("/") || path.contains(".") || path.contains("%") || path.contains(";")
                || path.contains("\\") || reserved(path, "/api") || reserved(path, "/assets")
                || reserved(path, "/webjars") || reserved(path, "/error")) return null;
        ModelAndView view = new ModelAndView("forward:/index.html");
        view.setStatus(HttpStatus.NOT_FOUND);
        return view;
    }

    private boolean acceptsHtml(HttpServletRequest request) {
        String accept = request.getHeader(HttpHeaders.ACCEPT);
        if (accept == null) return false;
        try {
            for (MediaType type : MediaType.parseMediaTypes(accept)) {
                // An API client sending */* has not asked for a browser document.
                if ("text".equalsIgnoreCase(type.getType()) && "html".equalsIgnoreCase(type.getSubtype())
                        && type.getQualityValue() > 0) return true;
            }
        } catch (IllegalArgumentException invalid) {
            return false;
        }
        return false;
    }

    private boolean reserved(String path, String root) {
        return path.equals(root) || path.startsWith(root + "/");
    }

    @Override
    public int getOrder() {
        return Ordered.HIGHEST_PRECEDENCE;
    }
}
