package com.transfer.tools.service;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.model.LocalTransferRequest;
import org.junit.Test;
import org.yaml.snakeyaml.Yaml;

import java.io.ByteArrayOutputStream;
import java.io.InputStream;
import java.nio.charset.StandardCharsets;
import java.util.Map;

import static org.junit.Assert.*;

public class OpenApiContractTest {
    @Test
    @SuppressWarnings("unchecked")
    public void multiNodeExampleIsAcceptedWithoutStorageId() throws Exception {
        try (InputStream input = getClass().getResourceAsStream("/static/openapi-local-transfer.yaml")) {
            assertNotNull(input);
            Map<String, Object> contract = new Yaml().load(input);
            Map<String, Object> components = (Map<String, Object>) contract.get("components");
            Map<String, Object> schemas = (Map<String, Object>) components.get("schemas");
            Map<String, Object> schema = (Map<String, Object>) schemas.get("LocalTransferRequest");
            LocalTransferRequest request = new ObjectMapper().convertValue(schema.get("example"), LocalTransferRequest.class);

            new EndpointNormalizationService().normalizeAndValidate(request);

            assertNull(request.getDestinationEndpoint().getStorageId());
            assertEquals(2, request.getDestinationEndpoint().getNodeBindings().size());
        }
    }

    @Test
    public void contractContainsAllLocalEndpointsAndEnums() throws Exception {
        InputStream input = getClass().getResourceAsStream("/static/openapi-local-transfer.yaml");
        assertNotNull(input);
        ByteArrayOutputStream output = new ByteArrayOutputStream();
        byte[] buffer = new byte[4096];
        int read;
        while ((read = input.read(buffer)) > 0) output.write(buffer, 0, read);
        String contract = new String(output.toByteArray(), StandardCharsets.UTF_8);
        for (String path : new String[]{"/api/master/local-nodes", "/api/master/local-transfers:",
                "/api/master/local-transfers/status", "/api/master/local-transfers/failures",
                "/api/master/local-transfers/stop", "/api/master/local-transfers/retry-failures",
                "/api/master/local-transfers/report", "/api/worker/local-capabilities",
                "/api/worker/local-transfers/validate", "/api/worker/local-transfers:"}) {
            assertTrue("missing " + path, contract.contains(path));
        }
        assertTrue(contract.contains("enum: [OSS_TO_OSS, OSS_TO_LOCAL, LOCAL_TO_OSS]"));
        assertTrue(contract.contains("application/zip"));
        assertFalse(contract.contains("verificationStatus"));
        assertFalse(contract.contains("checksumResult"));
    }
}
