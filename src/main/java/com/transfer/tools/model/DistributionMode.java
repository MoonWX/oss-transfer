package com.transfer.tools.model;

import com.fasterxml.jackson.annotation.JsonCreator;
import com.fasterxml.jackson.annotation.JsonValue;

public enum DistributionMode {
    AFFINITY("affinity"),
    BALANCED("balanced");

    private final String value;

    DistributionMode(String value) {
        this.value = value;
    }

    @JsonValue
    public String getValue() {
        return value;
    }

    @JsonCreator
    public static DistributionMode fromValue(String value) {
        if (value == null || value.trim().isEmpty()) return AFFINITY;
        for (DistributionMode mode : values()) {
            if (mode.value.equalsIgnoreCase(value)) return mode;
        }
        throw new IllegalArgumentException("unsupported distribution mode: " + value);
    }
}
