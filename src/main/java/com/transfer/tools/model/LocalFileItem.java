package com.transfer.tools.model;

import lombok.AllArgsConstructor;
import lombok.Data;

import java.nio.file.Path;

@Data
@AllArgsConstructor
public class LocalFileItem {
    private Path path;
    private String relativeKey;
    private long size;
    private long lastModified;
}
