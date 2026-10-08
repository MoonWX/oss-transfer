package com.transfer.tools.agent;

import org.springframework.core.Ordered;
import org.springframework.core.annotation.Order;
import org.springframework.stereotype.Component;
import javax.servlet.*;
import javax.servlet.http.*;
import java.io.IOException;

/** Enforce wire limits before Jackson allocates a base64 payload, including chunked requests. */
@Component
@Order(Ordered.HIGHEST_PRECEDENCE)
public class AgentPayloadLimitFilter implements Filter {
    @Override public void doFilter(ServletRequest input, ServletResponse output, FilterChain chain) throws IOException, ServletException {
        HttpServletRequest request = (HttpServletRequest) input;
        HttpServletResponse response = (HttpServletResponse) output;
        if (!request.getRequestURI().startsWith("/api/agents/")) { chain.doFilter(input, output); return; }
        long limit = request.getRequestURI().equals("/api/agents/result") || request.getRequestURI().equals("/api/agents/relay")
                ? 12 * 1024 * 1024 : 8192;
        response.setHeader("Cache-Control", "no-store");
        if (request.getContentLengthLong() > limit) { response.sendError(413, "Agent request is too large"); return; }
        chain.doFilter(new HttpServletRequestWrapper(request) {
            @Override public ServletInputStream getInputStream() throws IOException {
                ServletInputStream stream = request.getInputStream();
                return new ServletInputStream() {
                    private long count;
                    @Override public int read() throws IOException {
                        int value = stream.read();
                        if (value >= 0 && ++count > limit) throw new IOException("Agent request exceeds wire limit");
                        return value;
                    }
                    @Override public int read(byte[] bytes, int offset, int length) throws IOException {
                        int read = stream.read(bytes, offset, (int)Math.min(length, limit - count + 1));
                        if (read > 0 && (count += read) > limit) throw new IOException("Agent request exceeds wire limit");
                        return read;
                    }
                    @Override public boolean isFinished() { return stream.isFinished(); }
                    @Override public boolean isReady() { return stream.isReady(); }
                    @Override public void setReadListener(ReadListener listener) { stream.setReadListener(listener); }
                };
            }
        }, output);
    }
}
