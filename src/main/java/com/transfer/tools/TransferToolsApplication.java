package com.transfer.tools;

import org.springframework.boot.SpringApplication;
import org.springframework.boot.autoconfigure.SpringBootApplication;
import org.springframework.scheduling.annotation.EnableScheduling;

@SpringBootApplication
@EnableScheduling
public class TransferToolsApplication {

    public static void main(String[] args) {
        SpringApplication.run(TransferToolsApplication.class, args);
    }
}
